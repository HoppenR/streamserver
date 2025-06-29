package main

import (
	"context"
	"encoding/gob"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	ls "github.com/HoppenR/libstreams"
)

type Server struct {
	authData       *ls.AuthData
	follows        *ls.TwitchFollows
	hasInitStreams bool
	lives          map[string]ls.StreamData
	logger         *slog.Logger
	mutex          sync.Mutex
	onLive         func(ls.StreamData)
	onOffline      func(ls.StreamData)
	redirectUri    string
	srv            http.Server
	streams        *ls.Streams
	timer          time.Duration
	userName       string
	strimsEnabled  bool

	forceCheck chan bool
}

var ErrFollowsUnavailable = errors.New("No user access token and no follows obtained")

func NewServer() *Server {
	return &Server{
		forceCheck: make(chan bool),
		lives:      make(map[string]ls.StreamData),
		logger:     slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		streams:    new(ls.Streams),
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

func (bg *Server) SetInterval(timer time.Duration) *Server {
	bg.timer = timer
	bg.streams.RefreshInterval = timer
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

func (bg *Server) SetRedirect(redirectUri string) *Server {
	bg.redirectUri = redirectUri
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
	for eventLoopRunning {
		select {
		case interrupt := <-interruptCh:
			bg.logger.Warn("caught interrupt", "signal", interrupt)
			eventLoopRunning = false
			continue
		case <-bg.forceCheck:
			bg.logger.Info("recv force check")
			tick.Reset(bg.timer)
		case <-tick.C:
		}
		err = bg.check(false)
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
		bg.authData.FetchAppAccessToken()
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

func (bg *Server) serveData() {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("This endpoint is meant to be used through the streamchecker project"))
	})

	mux.HandleFunc("GET /auth", func(w http.ResponseWriter, r *http.Request) {
		if bg.authData.UserAccessToken == nil || bg.authData.UserAccessToken.IsExpired(bg.timer) {
			query := make(url.Values)
			query.Add("client_id", bg.authData.ClientID)
			query.Add("redirect_uri", bg.redirectUri)
			query.Add("response_type", "code")
			query.Add("scope", "user:read:follows")

			authURL := "https://id.twitch.tv/oauth2/authorize?" + query.Encode()
			http.Redirect(w, r, authURL, http.StatusFound)
			return
		}

		w.Write([]byte("Welcome to streamserver."))
	})

	mux.HandleFunc("GET /stream-data", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.Header.Get("Content-Type"), "application/octet-stream") {
			w.Write([]byte("This endpoint is meant to be used through the streamchecker project"))
			return
		}

		bg.logger.Info(
			"serving data",
			slog.String("ip", r.RemoteAddr),
			slog.String("x-real-ip", r.Header.Get("X-Real-IP")),
			slog.String("x-forwarded-for", r.Header.Get("X-Forwarded-For")),
		)

		if bg.authData.UserAccessToken == nil {
			http.Redirect(w, r, "/auth", http.StatusFound)
			return
		}

		bg.mutex.Lock()
		defer bg.mutex.Unlock()

		enc := gob.NewEncoder(w)
		err := enc.Encode(&bg.streams)
		if err != nil {
			http.Error(w, "Could not encode streams", http.StatusInternalServerError)
			return
		}
	})

	mux.HandleFunc("POST /stream-data", func(w http.ResponseWriter, r *http.Request) {
		if bg.authData.UserAccessToken == nil {
			http.Redirect(w, r, "/auth", http.StatusFound)
			return
		}
		bg.forceCheck <- true
	})

	mux.HandleFunc("GET /oauth-callback", func(w http.ResponseWriter, r *http.Request) {
		accessCode := r.URL.Query().Get("code")
		if accessCode == "" {
			http.Error(w, "Access token not found", http.StatusBadRequest)
			return
		}
		w.Write([]byte("Authentication successful! You can now close this page."))

		err := bg.authData.ExchangeCodeForUserAccessToken(accessCode, bg.redirectUri)
		if err != nil {
			bg.logger.Warn("could not exchange code for token", "err", err)
			return
		}
		bg.forceCheck <- true
	})

	bg.srv.Handler = mux
	bg.srv.ListenAndServe()
}

func (bg *Server) GetLiveStreams(refreshFollows bool) error {
	var err error
	// Twitch
	if bg.follows == nil || refreshFollows {
		if bg.authData.UserAccessToken != nil && !bg.authData.UserAccessToken.IsExpired(bg.timer) {
			newFollows := new(ls.TwitchFollows)
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
			bg.streams.Strims = newStrimsStreams
		}
	}

	bg.streams.LastFetched = time.Now()
	return nil
}
