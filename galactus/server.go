package galactus

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"github.com/automuteus/galactus/discord"
	"github.com/bwmarrin/discordgo"
	"github.com/go-redis/redis/v8"
	"github.com/gorilla/mux"
	"io/ioutil"
	"log"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const BroadcastToClientCapturesTimeout = time.Millisecond * 200
const AckFromClientCapturesTimeout = time.Second

var ctx = context.Background()

type TokenProvider struct {
	client         *redis.Client
	primarySession *discordgo.Session

	//maps hashed tokens to active discord sessions
	activeSessions map[string]*discordgo.Session
	sessionLock    sync.RWMutex
}

func guildTokensKey(guildID string) string {
	return "automuteus:tokens:" + guildID
}

func allTokensKey() string {
	return "automuteus:tokens"
}

func guildTokenLock(guildID, hToken string) string {
	return "automuteus:lock:" + hToken + ":" + guildID
}

func NewTokenProvider(botToken, redisAddr, redisUser, redisPass string) *TokenProvider {
	rdb := redis.NewClient(&redis.Options{
		Addr:     redisAddr,
		Username: redisUser,
		Password: redisPass,
		DB:       0, // use default DB
	})
	dg, err := discordgo.New("Bot " + botToken)
	if err != nil {
		log.Fatal(err)
	}
	err = dg.Open()
	if err != nil {
		log.Fatal(err)
	}

	return &TokenProvider{
		client:         rdb,
		primarySession: dg,
		activeSessions: make(map[string]*discordgo.Session),
		sessionLock:    sync.RWMutex{},
	}
}

func (tokenProvider *TokenProvider) PopulateAndStartSessions() {
	keys, err := tokenProvider.client.HGetAll(ctx, allTokensKey()).Result()
	if err != nil {
		log.Println(err)
		return
	}

	for _, v := range keys {
		tokenProvider.openAndStartSessionWithToken(v)
	}
}

func (tokenProvider *TokenProvider) openAndStartSessionWithToken(token string) bool {
	k := hashToken(token)
	tokenProvider.sessionLock.Lock()
	defer tokenProvider.sessionLock.Unlock()

	if _, ok := tokenProvider.activeSessions[k]; !ok {
		sess, err := discordgo.New("Bot " + token)
		if err != nil {
			log.Println(err)
			return false
		}
		err = sess.Open()
		if err != nil {
			log.Println(err)
			return false
		}
		//associates the guilds with this token to be used for requests
		sess.AddHandler(tokenProvider.newGuild())
		log.Println("Opened session on startup for " + k)
		tokenProvider.activeSessions[k] = sess
		return true
	}
	return false
}

func (tokenProvider *TokenProvider) getAnySession(guildID string) (*discordgo.Session, string) {
	hTokens, err := tokenProvider.client.SMembers(context.Background(), guildTokensKey(guildID)).Result()
	if err != nil {
		return nil, ""
	}

	tokenProvider.sessionLock.RLock()
	defer tokenProvider.sessionLock.RUnlock()

	for _, hToken := range hTokens {
		//if this token isn't potentially rate-limited
		if tokenProvider.CanUseGuildTokenCombo(guildID, hToken) {
			if sess, ok := tokenProvider.activeSessions[hToken]; ok {
				return sess, hToken
			} else {
				//remove this key from our records and keep going
				tokenProvider.client.SRem(context.Background(), guildTokensKey(guildID), hToken)
			}
		} else {
			log.Println("Secondary token is potentially rate-limited. Skipping")
		}
	}

	return nil, ""
}

func (tokenProvider *TokenProvider) IncrGuildTokenComboLock(guildID, hashToken string) {
	v := int64(0)
	vStr, _ := tokenProvider.client.Get(context.Background(), guildTokenLock(guildID, hashToken)).Result()
	v, _ = strconv.ParseInt(vStr, 10, 64)
	v++

	//5 second TTL
	tokenProvider.client.Set(context.Background(), guildTokenLock(guildID, hashToken), v, time.Second*5)
}

func (tokenProvider *TokenProvider) CanUseGuildTokenCombo(guildID, hashToken string) bool {
	res, err := tokenProvider.client.Get(context.Background(), guildTokenLock(guildID, hashToken)).Result()
	if err != nil {
		return true
	}
	i, err := strconv.ParseInt(res, 10, 64)
	if err != nil {
		return true
	}

	return i < 5
}

func (tokenProvider *TokenProvider) Run(port string) {
	r := mux.NewRouter()

	// /modify/guild/conncode/userid?mute=true?deaf=false
	r.HandleFunc("/modify/{guildID}/{connectCode}/{userID}", func(w http.ResponseWriter, r *http.Request) {
		vars := mux.Vars(r)
		guildID := vars["guildID"]
		userID := vars["userID"]
		connectCode := vars["connectCode"]
		m := r.URL.Query().Get("mute")
		d := r.URL.Query().Get("deaf")
		gid, gerr := strconv.ParseInt(guildID, 10, 64)
		uid, uerr := strconv.ParseInt(userID, 10, 64)
		if m == "" || d == "" || gerr != nil || uerr != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Invalid values or parameters received. Query should be of the form `/modify/<guildID>/<conncode>/<userID>?mute=true?deaf=false`"))
			return
		}

		sess, hToken := tokenProvider.getAnySession(guildID)
		if sess != nil {
			err := discord.ApplyMuteDeaf(sess, guildID, userID, m == "true" || m == "t", d == "true" || d == "t")
			if err == nil {
				tokenProvider.IncrGuildTokenComboLock(guildID, hToken)
				log.Printf("Successfully applied mute=%v, deaf=%v using secondary bot\n", m, d)
				w.WriteHeader(http.StatusOK)
				return
			}
		} else {
			log.Println("No secondary bot tokens found. Trying other methods")
		}

		//this is cheeky, but use the connect code as part of the lock; don't issue too many requests on the capture client w/ this code
		if tokenProvider.CanUseGuildTokenCombo(guildID, connectCode) {
			//if the secondary token didn't work, then next we try the client-side capture request
			task := discord.NewModifyTask(gid, uid, discord.NoNickPatchParams{
				Deaf: d == "true" || d == "t",
				Mute: m == "true" || m == "t",
			})
			jBytes, err := json.Marshal(task)
			if err != nil {
				log.Println(err)
				w.WriteHeader(http.StatusBadRequest)
				w.Write([]byte("Unable to marshal JSON into Task"))
				return
			} else {
				acked := make(chan bool)
				pubsub := tokenProvider.client.Subscribe(context.Background(), discord.BroadcastTaskAckKey(task.TaskID))
				go tokenProvider.waitForAck(pubsub, BroadcastToClientCapturesTimeout, acked)

				err := tokenProvider.client.Publish(context.Background(), discord.TasksSubscribeKey(connectCode), jBytes).Err()
				if err != nil {
					log.Println(err)
				}

				res := <-acked
				if !res {
					log.Println("Request timed out waiting for broadcast to capture clients for task " + task.TaskID)
					//falls through to using official bot token below
				} else {
					acked := make(chan bool)
					pubsub := tokenProvider.client.Subscribe(context.Background(), discord.CompleteTaskAckKey(task.TaskID))
					go tokenProvider.waitForAck(pubsub, AckFromClientCapturesTimeout, acked)
					res := <-acked
					if res {
						log.Println("Successful mute/deafen using client capture bot!")
						tokenProvider.IncrGuildTokenComboLock(guildID, connectCode)
						//hooray! we did the mute with a client token!
						w.WriteHeader(http.StatusOK)
						return
					} else {
						log.Println("No ack from client capture bot for task " + task.TaskID)
						//falls through to using official bot token below
					}
				}
			}
		} else {
			log.Println("Capture client is probably rate-limited. Deferring to main bot instead")
		}

		log.Printf("Applying mute=%v, deaf=%v using primary bot\n", m, d)
		err := discord.ApplyMuteDeaf(tokenProvider.primarySession, guildID, userID, m == "true" || m == "t", d == "true" || d == "t")
		if err == nil {
			w.WriteHeader(http.StatusOK)
			return
		} else {
			w.WriteHeader(http.StatusInternalServerError)
			return
		}
	}).Methods("POST")

	r.HandleFunc("/addtoken", func(w http.ResponseWriter, r *http.Request) {
		body, err := ioutil.ReadAll(r.Body)
		if err != nil {
			log.Println(err)
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}
		defer r.Body.Close()

		token := string(body)

		sess, err := discordgo.New("Bot " + token)
		if err != nil {
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte(err.Error()))
			return
		}
		err = sess.Open()
		if err != nil {
			w.WriteHeader(http.StatusUnauthorized)
			w.Write([]byte(err.Error()))
			return
		}

		k := hashToken(token)
		tokenProvider.sessionLock.RLock()
		if _, ok := tokenProvider.activeSessions[k]; ok {
			log.Println("Key already exists on the server")
			w.WriteHeader(http.StatusBadRequest)
			w.Write([]byte("Key already exists on the server"))
			tokenProvider.sessionLock.RUnlock()
			return
		}
		tokenProvider.sessionLock.RUnlock()

		hash := hashToken(token)
		tokenProvider.sessionLock.Lock()
		tokenProvider.activeSessions[hash] = sess
		tokenProvider.sessionLock.Unlock()

		sess.AddHandler(tokenProvider.newGuild())
		err = tokenProvider.client.HSet(ctx, allTokensKey(), hash, token).Err()
		if err != nil {
			log.Println(err)
		}

		//TODO handle guild removals?
		for _, v := range sess.State.Guilds {
			err := tokenProvider.client.SAdd(ctx, guildTokensKey(v.ID), hash).Err()
			if err != redis.Nil {
				log.Println(err)
			} else {
				log.Println("Added token for guild " + v.ID)
			}
		}
	}).Methods("POST")

	r.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
	}).Methods("GET")

	log.Println("Galactus token service is running on port " + port + "...")
	http.ListenAndServe(":"+port, r)
}

func (tokenProvider *TokenProvider) waitForAck(pubsub *redis.PubSub, waitTime time.Duration, result chan<- bool) {
	t := time.NewTimer(waitTime)
	defer pubsub.Close()

	for {
		select {
		case <-t.C:
			result <- false
			return
		case t := <-pubsub.Channel():
			result <- t.Payload == "true"
			return
		}
	}
}

func hashToken(token string) string {
	h := sha256.New()
	h.Sum([]byte(token))
	return hex.EncodeToString(h.Sum(nil))
}

func (tokenProvider *TokenProvider) Close() {
	tokenProvider.sessionLock.Lock()
	for _, v := range tokenProvider.activeSessions {
		v.Close()
	}

	tokenProvider.activeSessions = map[string]*discordgo.Session{}
	tokenProvider.sessionLock.Unlock()
	tokenProvider.primarySession.Close()
}

func (tokenProvider *TokenProvider) newGuild() func(s *discordgo.Session, m *discordgo.GuildCreate) {
	return func(s *discordgo.Session, m *discordgo.GuildCreate) {
		tokenProvider.sessionLock.RLock()
		for hashedToken, sess := range tokenProvider.activeSessions {
			if sess == s {
				err := tokenProvider.client.SAdd(ctx, guildTokensKey(m.Guild.ID), hashedToken)
				if err != nil {
					log.Println(err)
				} else {
					log.Println("Token added for running guild " + m.Guild.ID)
				}
			}
		}

		tokenProvider.sessionLock.RUnlock()
	}
}
