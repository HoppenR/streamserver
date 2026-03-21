package main

import (
	"context"
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
	"strings"
	"sync"
	"syscall"
	"time"

	ls "github.com/HoppenR/libstreams"
)

type Server struct {
	onLive         func(ls.StreamData)
	streams        *ls.Streams
	forceCheck     chan bool
	lives          map[string]ls.StreamData
	logger         *slog.Logger
	onOffline      func(ls.StreamData)
	authData       *ls.AuthData
	follows        *ls.TwitchFollows
	redirectURI    string
	srv            http.Server
	timer          time.Duration
	mutex          sync.Mutex
	strimsEnabled  bool
	hasInitStreams bool
	htmlTemplate   *template.Template
	basicAuthUser  string
	basicAuthPass  string
	lastFetched    time.Time
}

type dashboardView struct {
	*ls.Streams
	LastFetched            time.Time
	RefreshIntervalSeconds int
}

var ErrFollowsUnavailable = errors.New("no user access token and no follows obtained")

const dashboardTmpl = `
<!DOCTYPE html>
<html lang="en">
<head>
    <meta charset="UTF-8">
    <meta name="viewport" content="width=device-width, initial-scale=1.0">
    <title>Streamserver Dashboard</title>
    <style>
        body { font-family: -apple-system, BlinkMacSystemFont, "Segoe UI", Helvetica, Arial, sans-serif;
               max-width: 800px; margin: 2em auto; line-height: 1.6; background: #0d1117; color: #c9d1d9; padding: 0 1em; }
        a { color: #58a6ff; text-decoration: none; font-weight: 500; }
	.lists-container { display: flex; gap: 2em; flex-wrap: wrap; }
        .list-column { flex: 1; min-width: 300px; }
        .stream-item { padding: 0.75em 0; border-bottom: 1px solid #30363d; display: flex; justify-content: space-between; align-items: center; }
        .viewer-count { color: #3fb950; font-family: monospace; background: rgba(63, 185, 80, 0.1); padding: 2px 8px; border-radius: 6px; }
        .meta { color: #8b949e; font-size: 0.85em; margin-bottom: 2em; }
    </style>
</head>
<body>
    <h1>Streamserver Status</h1>
    <div class="meta">
        Last updated: {{.LastFetched.Format "2006-01-02 15:04:05 (MST)"}}
        &mdash; <span id="timer">Next update in ...s</span>
    </div>

    <script>
        const config = {
            lastModified: {{.LastFetched.Unix}},
            refreshInterval: {{.RefreshIntervalSeconds}}
        }
        function startFetchTimer() {
            const display = document.getElementById('timer');
            const update = () => {
                const now = Math.floor(Date.now() / 1000);
                const nextFetchAt = config.lastModified + config.refreshInterval;
                let remaining = nextFetchAt - now;
                if (remaining <= 0) {
                    display.innerText = "Syncing with server...";
		    location.reload();
                } else {
                    display.innerText = "Next update in " + remaining + "s";
                }
            };
            update();
            setInterval(update, 1000);
        }
        window.onload = startFetchTimer;
    </script>

    <div class="lists-container">
        <div class="list-column">
            <h2>Twitch ({{.Twitch.Len}})</h2>
            <ul class="stream-list">
                {{range .Twitch.Data}}
                <li class="stream-item">
                    <a href="https://twitch.tv/{{.UserName}}" target="_blank" rel="noopener">{{.UserName}}</a>
                    <span class="viewer-count">{{.ViewerCount}} viewers</span>
                </li>
                {{end}}
	    </ul>
        </div>

        <div class="list-column">
            <h2>Strims ({{.Strims.Len}})</h2>
            <ul class="stream-list">
                {{range .Strims.Data}}
                <li class="stream-item">
                    <a href="https://strims.gg{{.URL}}" target="_blank" rel="noopener">{{.Channel}}</a>
                    <span class="viewer-count">{{.Viewers}} viewers</span>
                </li>
                {{end}}
	    </ul>
        </div>
    </div>
</body>
</html>`

func NewServer() *Server {
	return &Server{
		forceCheck:   make(chan bool),
		lives:        make(map[string]ls.StreamData),
		logger:       slog.New(slog.NewJSONHandler(os.Stdout, nil)),
		streams:      new(ls.Streams),
		htmlTemplate: template.Must(template.New("dashboard").Parse(dashboardTmpl)),
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

func (bg *Server) SetBasicAuthCredentials(user, pass string) *Server {
	bg.basicAuthUser = user
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
		if !ok || user != bg.basicAuthUser || pass != bg.basicAuthPass {
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
			query := make(url.Values)
			query.Add("client_id", bg.authData.ClientID)
			query.Add("redirect_uri", bg.redirectURI)
			query.Add("response_type", "code")
			query.Add("scope", "user:read:follows")

			authURL := "https://id.twitch.tv/oauth2/authorize?" + query.Encode()
			http.Redirect(w, r, authURL, http.StatusFound)
			return
		}

		_, _ = w.Write([]byte("Welcome to streamserver."))
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
		accessCode := r.URL.Query().Get("code")
		if accessCode == "" {
			http.Error(w, "Access token not found", http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte("Authentication successful! You can now close this page."))

		err := bg.authData.ExchangeCodeForUserAccessToken(accessCode, bg.redirectURI)
		if err != nil {
			bg.logger.Warn("could not exchange code for token", "err", err)
			return
		}
		bg.forceCheck <- true
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

	bg.lastFetched = time.Now().Truncate(time.Second)
	return nil
}
