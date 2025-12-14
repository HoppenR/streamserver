package main

import (
	"flag"
	"fmt"
	"log/slog"
	"os"
	"time"

	ls "github.com/HoppenR/libstreams"
)

const (
	ConfigFile  = "config.json"
	CacheFolder = "streamserver"
)

func main() {
	address := flag.String(
		"a",
		"http://0.0.0.0:8181",
		"Address of the server",
	)
	redirect := flag.String(
		"e",
		"http://localhost:8181/oauth-callback",
		"Callback address for authenticating",
	)
	refreshTime := flag.Duration(
		"r",
		5*time.Minute,
		"How often the daemon refreshes the data",
	)
	useCache := flag.Bool(
		"u",
		true,
		"Use cache, set to false to refresh cache (useful after making changes to config.json)",
	)
	flag.Usage = func() {
		fmt.Fprintf(
			flag.CommandLine.Output(),
			"Usage: %s [-a=ADDRESS] [-r=DURATION] [-u=false]\n",
			os.Args[0],
		)
		flag.PrintDefaults()
	}
	flag.Parse()

	if flag.NArg() > 0 {
		flag.Usage()
		os.Exit(2)
	}

	var err error

	logger := slog.New(slog.NewTextHandler(os.Stdout, nil))

	cfg := new(Config)
	err = cfg.SetConfigFolder("streamserver")
	if err != nil {
		logger.Error("error making config folder", "err", err)
		return
	}
	configErr := cfg.Load(ConfigFile)
	if configErr != nil {
		logger.Warn("config read failed", "err", configErr)
		err = cfg.GetFromEnv()
		if err != nil {
			logger.Error("error reading env", "err", err)
			return
		}
		err = cfg.Save(ConfigFile)
		if err != nil {
			logger.Error("error saving config", "err", err)
			return
		}
	} else {
		logger.Info("read config data")
	}

	ad := new(ls.AuthData)
	ad.SetClientID(cfg.data.ClientID)
	ad.SetClientSecret(cfg.data.ClientSecret)
	ad.SetUserName(cfg.data.UserName)
	err = ad.SetCacheFolder(CacheFolder)
	if err != nil {
		logger.Error("error making cache folder", "err", err)
		return
	}
	if *useCache {
		err = ad.GetCachedData()
		if err != nil {
			logger.Warn("cache read failed", "err", configErr)
		} else {
			logger.Info("read cached data")
		}
	}

	srv := NewServer()
	srv.SetAddress(*address)
	srv.SetAuthData(ad)
	srv.SetInterval(*refreshTime)
	srv.SetRedirect(*redirect)
	srv.SetLogger(logger)
	srv.EnableStrims(true)
	err = srv.Run()
	if err != nil {
		logger.Error("server exited abnormally", "err", err)
		return
	}
	err = ad.SaveCachedData()
	if err != nil {
		logger.Error("error saving cache", "err", err)
		return
	}
}
