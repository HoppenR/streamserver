package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	data         configData
	configFolder string
}

type configData struct {
	ClientID     string `json:"client_id"`
	ClientSecret string `json:"client_secret"`
	UserName     string `json:"user_name"`
}

var ErrConfigFolderUnset = errors.New("config folder not set")

func (c *Config) SetConfigFolder(name string) error {
	confdir, err := os.UserConfigDir()
	if err != nil {
		return err
	}
	abspath := filepath.Join(confdir, name)
	err = os.MkdirAll(abspath, os.ModePerm)
	if err != nil {
		return err
	}
	c.configFolder = abspath
	return nil
}

func (c *Config) Load(filename string) error {
	if c.configFolder == "" {
		return ErrConfigFolderUnset
	}
	file, err := os.Open(filepath.Join(c.configFolder, filename))
	if err != nil {
		return err
	}
	defer file.Close()
	dec := json.NewDecoder(file)
	err = dec.Decode(&c.data)
	if err != nil {
		return err
	}
	return nil
}

func (c *Config) GetFromEnv() error {
	clientID := strings.TrimSpace(os.Getenv("CLIENT_ID"))
	clientSecret := strings.TrimSpace(os.Getenv("CLIENT_SECRET"))
	userName := strings.TrimSpace(os.Getenv("USER_NAME"))

	var missing []string
	if clientID == "" {
		missing = append(missing, "CLIENT_ID")
	}
	if clientSecret == "" {
		missing = append(missing, "CLIENT_SECRET")
	}
	if userName == "" {
		missing = append(missing, "USER_NAME")
	}

	if len(missing) > 0 {
		return fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}

	c.data.ClientID = clientID
	c.data.ClientSecret = clientSecret
	c.data.UserName = userName
	return nil
}

func (c *Config) Save(filename string) error {
	if c.configFolder == "" {
		return ErrConfigFolderUnset
	}
	file, err := os.Create(filepath.Join(c.configFolder, filename))
	if err != nil {
		return err
	}
	defer file.Close()
	enc := json.NewEncoder(file)
	enc.SetIndent("", "    ")
	enc.Encode(c.data)
	return nil
}
