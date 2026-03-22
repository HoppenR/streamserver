package main

import (
	"context"
	"crypto/rand"
	_ "embed"
	"encoding/gob"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"

	ls "github.com/HoppenR/libstreams"
)

type Server struct {
	onLive          func(ls.StreamData)
	streams         *ls.Streams
	forceCheck      chan bool
	lives           map[string]ls.StreamData
	logger          *slog.Logger
	onOffline       func(ls.StreamData)
	authData        *ls.AuthData
	follows         *ls.TwitchFollows
	redirectURI     string
	srv             http.Server
	timer           time.Duration
	mutex           sync.Mutex
	strimsEnabled   bool
	hasInitStreams  bool
	htmlTemplate    *template.Template
	basicAuthPass   string
	lastFetched     time.Time
	stateSignSecret []byte
}

type dashboardView struct {
	*ls.Streams
	LastFetched            time.Time
	RefreshIntervalSeconds int
}

var ErrFollowsUnavailable = errors.New("no user access token and no follows obtained")

//go:embed dashboard.gohtml
var dashboardTmpl string

func NewServer() *Server {
	secret := make([]byte, 32)
	n, err := rand.Read(secret)
	if err != nil || n != 32 {
		panic("failed to seed signing secret: " + err.Error())
	}

	return &Server{
		forceCheck:   make(chan bool),
		htmlTemplate: template.Must(template.New("dashboard").Parse(dashboardTmpl)),
		lives:        make(map[string]ls.StreamData),
		logger:       slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		srv: http.Server{
			ReadTimeout:       5 * time.Second,
			ReadHeaderTimeout: 2 * time.Second,
			WriteTimeout:      10 * time.Second,
			IdleTimeout:       120 * time.Second,
		},
		stateSignSecret: secret,
		streams:         new(ls.Streams),
	}
}

func (bg *Server) SetAddress(address string) *Server {
	bg.srv.Addr = address
	return bg
}

func (bg *Server) SetAuthData(ad *ls.AuthData) *Server {
	bg.authData = ad
	return bg
}

func (bg *Server) SetBasicAuthPassword(pass string) *Server {
	bg.basicAuthPass = pass
	return bg
}

func (bg *Server) SetInterval(timer time.Duration) *Server {
	bg.timer = timer
	return bg
}

func (bg *Server) SetLiveCallback(f func(ls.StreamData)) *Server {
	bg.onLive = f
	return bg
}

func (bg *Server) SetLogger(logger *slog.Logger) *Server {
	bg.logger = logger
	return bg
}

func (bg *Server) SetOfflineCallback(f func(ls.StreamData)) *Server {
	bg.onOffline = f
	return bg
}

func (bg *Server) SetRedirect(redirectURI string) *Server {
	bg.redirectURI = redirectURI
	return bg
}

func (bg *Server) EnableStrims(enable bool) *Server {
	bg.strimsEnabled = enable
	return bg
}

func (bg *Server) Run() error {
	var err error
	err = bg.authData.GetAppAccessToken()
	if err != nil {
		return err
	}
	err = bg.authData.GetUserID()
	if err != nil {
		return err
	}
	err = bg.check(false)
	if err != nil {
		return err
	}
	// Http server
	if bg.srv.Addr != "" {
		go bg.serveData()
	} else {
		bg.logger.Warn("address unset, server not running")
	}
	// Interrupt handling
	interruptCh := make(chan os.Signal, 1)
	signal.Notify(interruptCh, os.Interrupt, syscall.SIGTERM)
	// Main Event Loop
	tick := time.NewTicker(bg.timer)
	eventLoopRunning := true
	var refreshFollows bool
	for eventLoopRunning {
		refreshFollows = false
		select {
		case interrupt := <-interruptCh:
			bg.logger.Warn("caught interrupt", "signal", interrupt)
			eventLoopRunning = false
			continue
		case b := <-bg.forceCheck:
			refreshFollows = b
			bg.logger.Info("force check", "refreshFollows", refreshFollows)
			tick.Reset(bg.timer)
		case <-tick.C:
		}
		err = bg.check(refreshFollows)
		if err != nil {
			return err
		}
	}
	// Cleanup
	err = bg.srv.Close()
	if !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

func (bg *Server) check(refreshFollows bool) error {
	var err error
	bg.mutex.Lock()
	defer bg.mutex.Unlock()

	if bg.authData.AppAccessToken == nil || bg.authData.AppAccessToken.IsExpired(bg.timer) {
		bg.logger.Info("refreshing app access token")
		err = bg.authData.FetchAppAccessToken()
		if err != nil {
			bg.logger.Warn("fetching app access token failed")
		}
	}
	if bg.authData.UserAccessToken != nil && bg.authData.UserAccessToken.IsExpired(bg.timer) {
		bg.logger.Info("refreshing user access token")
		err = bg.authData.RefreshUserAccessToken()
		if errors.Is(err, ls.ErrUnauthorized) {
			bg.logger.Warn("refresh user access token failed")
			bg.authData.UserAccessToken = nil
		} else if err != nil {
			return err
		}
	}
	err = bg.GetLiveStreams(refreshFollows)
	if errors.Is(err, ErrFollowsUnavailable) {
		return nil
	} else if err != nil {
		return err
	}

	if bg.onLive != nil || bg.onOffline != nil {
		bg.doStreamStatusCallbacks()
	}
	return nil
}

func (bg *Server) doStreamStatusCallbacks() {
	newLives := make(map[string]ls.StreamData)
	for i, v := range bg.streams.Strims.Data {
		newLives[strings.ToLower(v.Channel)] = &bg.streams.Strims.Data[i]
	}
	for i, v := range bg.streams.Twitch.Data {
		newLives[strings.ToLower(v.UserName)] = &bg.streams.Twitch.Data[i]
	}
	if bg.hasInitStreams {
		if bg.onLive != nil {
			for user, data := range newLives {
				if _, ok := bg.lives[user]; !ok {
					bg.onLive(data)
				}
			}
		}
		if bg.onOffline != nil {
			for user, data := range bg.lives {
				if _, ok := newLives[user]; !ok {
					bg.onOffline(data)
				}
			}
		}
	} else {
		bg.hasInitStreams = true
	}
	bg.lives = newLives
}

func (bg *Server) basicAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if bg.basicAuthPass == "" {
			next(w, r)
			return
		}
		user, pass, ok := r.BasicAuth()
		if !ok || user != bg.authData.UserName || pass != bg.basicAuthPass {
			w.Header().Set("WWW-Authenticate", `Basic realm="restricted", charset="UTF-8"`)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

func (bg *Server) serveData() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("This endpoint is meant to be used through the streamchecker project"))
	})

	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		if bg.authData.UserAccessToken == nil || bg.authData.UserAccessToken.IsExpired(bg.timer) {
			stateContent, err := createSignedState(bg.stateSignSecret)
			if err != nil {
				bg.logger.Error("failed to generate state", "err", err)
				http.Error(w, "Internal server error", http.StatusInternalServerError)
				return
			}

			bg.logger.Info(
				"authenticating user",
				slog.String("ip", r.RemoteAddr),
				slog.String("x-forwarded-for", r.Header.Get("X-Forwarded-For")),
			)

			query := make(url.Values)
			query.Add("client_id", bg.authData.ClientID)
			query.Add("redirect_uri", bg.redirectURI)
			query.Add("response_type", "code")
			query.Add("scope", "user:read:follows")
			query.Add("state", stateContent)
			http.SetCookie(w, &http.Cookie{
				Name:     "oauth_state",
				Value:    stateContent,
				Path:     "/",
				HttpOnly: true,
				Secure:   true,
				SameSite: http.SameSiteLaxMode,
				MaxAge:   60,
			})

			authURL := "https://id.twitch.tv/oauth2/authorize?" + query.Encode()
			http.Redirect(w, r, authURL, http.StatusFound)
			return
		}

		http.Error(w, "Already authenticated", http.StatusForbidden)
	})

	mux.HandleFunc("GET /stream-data", bg.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		if bg.authData.UserAccessToken == nil || bg.authData.UserAccessToken.IsExpired(bg.timer) {
			http.Redirect(w, r, "/auth", http.StatusFound)
			return
		}

		bg.logger.Info(
			"serving data",
			slog.String("ip", r.RemoteAddr),
			slog.String("x-forwarded-for", r.Header.Get("X-Forwarded-For")),
		)

		bg.mutex.Lock()
		defer bg.mutex.Unlock()

		w.Header().Set("Last-Modified", bg.lastFetched.UTC().Format(http.TimeFormat))
		w.Header().Set("Cache-Control", fmt.Sprintf("public, max-age=%d", int(bg.timer.Seconds())))

		if ims := r.Header.Get("If-Modified-Since"); ims != "" {
			t, err := http.ParseTime(ims)
			if err == nil && !bg.lastFetched.After(t) {
				w.WriteHeader(http.StatusNotModified)
				return
			}
		}

		accept := r.Header.Get("Accept")
		var err error
		if strings.Contains(accept, "application/octet-stream") {
			w.Header().Set("Content-Type", "application/octet-stream")
			enc := gob.NewEncoder(w)
			err = enc.Encode(bg.streams)
		} else if strings.Contains(accept, "application/json") {
			w.Header().Set("Content-Type", "application/json")
			enc := json.NewEncoder(w)
			err = enc.Encode(bg.streams)
		} else {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			dbw := &dashboardView{
				Streams:                bg.streams,
				LastFetched:            bg.lastFetched,
				RefreshIntervalSeconds: int(bg.timer.Seconds()),
			}
			err = bg.htmlTemplate.Execute(w, dbw)
		}
		if err != nil {
			http.Error(w, "Could not encode streams", http.StatusInternalServerError)
			return
		}
	}))

	mux.HandleFunc("POST /stream-data", bg.basicAuth(func(w http.ResponseWriter, r *http.Request) {
		if bg.authData.UserAccessToken == nil || bg.authData.UserAccessToken.IsExpired(bg.timer) {
			http.Error(w, "Unauthorized: Twitch login required", http.StatusUnauthorized)
			return
		}
		bg.forceCheck <- true
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("Update triggered"))
	}))

	mux.HandleFunc("GET /oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		stateCookie, err := r.Cookie("oauth_state")
		if err != nil {
			http.Error(w, "Forbidden: Invalid or expired state", http.StatusForbidden)
			return
		}
		stateQuery := r.URL.Query().Get("state")
		if stateQuery != stateCookie.Value {
			http.Error(w, "Forbidden: Invalid or expired state", http.StatusForbidden)
			return
		}
		if !verifySignedState(stateQuery, bg.stateSignSecret) {
			bg.logger.Warn(
				"unauthorized or expired oauth callback",
				slog.String("ip", r.RemoteAddr),
				slog.String("x-forwarded-for", r.Header.Get("X-Forwarded-For")),
			)
			http.Error(w, "Forbidden: Invalid or expired state", http.StatusForbidden)
			return
		}
		http.SetCookie(w, &http.Cookie{
			Name:   "oauth_state",
			Value:  "",
			Path:   "/",
			MaxAge: -1,
		})

		accessCode := r.URL.Query().Get("code")
		if accessCode == "" {
			http.Error(w, "Access token not found", http.StatusBadRequest)
			return
		}
		token, err := bg.authData.ExchangeCodeForUserAccessToken(accessCode, bg.redirectURI)
		if err != nil {
			bg.logger.Warn("could not exchange code for token", "err", err)
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
		val, err := bg.authData.ValidateUserAccessToken(token)
		if err != nil {
			bg.logger.Warn("could not validate token", "err", err)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}
		if val.Login != bg.authData.UserName {
			bg.logger.Warn("identity mismatch", "got", val.Login, "want", bg.authData.UserName)
			http.Error(w, "Unauthorized", http.StatusUnauthorized)
			return
		}

		bg.authData.UserAccessToken = token

		_, _ = w.Write([]byte("Authentication successful! You can now close this page."))
		bg.forceCheck <- false
	})

	bg.srv.Handler = mux
	if err := bg.srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		bg.logger.Error("listen and serve failed", "err", err)
	}
}

func (bg *Server) GetLiveStreams(refreshFollows bool) error {
	var err error
	// Twitch
	if bg.follows == nil || refreshFollows {
		if bg.authData.UserAccessToken != nil && !bg.authData.UserAccessToken.IsExpired(bg.timer) {
			var newFollows *ls.TwitchFollows
			newFollows, err = ls.GetTwitchFollows(bg.authData.UserAccessToken.AccessToken, bg.authData.ClientID, bg.authData.UserID)
			if errors.Is(err, context.DeadlineExceeded) {
				bg.logger.Warn("timed out getting twitch follows")
			} else if errors.Is(err, ls.ErrUnauthorized) {
				bg.logger.Warn("unauthorized getting follows")
				bg.authData.UserAccessToken = nil
			} else if err != nil {
				return err
			}
			bg.follows = newFollows
		}

		if bg.follows == nil {
			bg.logger.Warn("no follows obtained")
			return ErrFollowsUnavailable
		}
	}

	var newTwitchStreams *ls.TwitchStreams
	newTwitchStreams, err = ls.GetLiveTwitchStreams(bg.authData.AppAccessToken.AccessToken, bg.authData.ClientID, bg.follows)
	if errors.Is(err, context.DeadlineExceeded) {
		bg.logger.Warn("timed out getting twitch follows")
	} else if errors.Is(err, ls.ErrUnauthorized) {
		bg.logger.Warn("unauthorized getting twitch streams")
		bg.authData.AppAccessToken = nil
	} else if err != nil {
		return err
	} else {
		sort.Sort(sort.Reverse(newTwitchStreams))
		bg.streams.Twitch = newTwitchStreams
	}

	// Strims
	if bg.strimsEnabled {
		var newStrimsStreams *ls.StrimsStreams
		newStrimsStreams, err = ls.GetLiveStrimsStreams()
		if errors.Is(err, context.DeadlineExceeded) {
			bg.logger.Warn("timed out getting strims streams")
		} else if err != nil {
			return err
		} else {
			sort.Sort(sort.Reverse(newStrimsStreams))
			bg.streams.Strims = newStrimsStreams
		}
	}

	bg.lastFetched = time.Now().Truncate(time.Second)
	return nil
}
