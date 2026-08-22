package config

import (
	"os"

	"gopkg.in/yaml.v2"
)

type FoundryConfig struct {
	Directory   string `yaml:"directory"`
	ServicePath string `yaml:"service_path"`
}

type DBConfig struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

// GoogleConfig points at the OAuth 2.0 client used to talk to the Google Meet
// API.
//
// This is an OAuth *client*, not an API key: the Meet API has no API-key path,
// and a plain service account only works on a Workspace domain, so on a
// consumer account the bot has to act as a real Google user who granted consent
// once.
//
// Two of these fields are inputs and one is an output, which is worth stating
// plainly because they are easy to confuse:
//
//   - CredentialsFile is the JSON downloaded from the Google Cloud Console when
//     the "Desktop app" OAuth client is created. Point at it and the client ID
//     and secret are read out of it; there is nothing to copy by hand.
//   - ClientID / ClientSecret are the same two values supplied inline instead,
//     for anyone who would rather not keep the file around. They take priority
//     if both are given.
//   - TokenFile is written BY the bot: it is where the refresh token obtained
//     from consent gets cached, relative to the storage base path. It does not
//     exist until the first successful sign-in, and it is not something to
//     download from anywhere.
type GoogleConfig struct {
	CredentialsFile string `yaml:"credentials_file"`
	ClientID        string `yaml:"client_id"`
	ClientSecret    string `yaml:"client_secret"`
	TokenFile       string `yaml:"token_file"`
}

type Config struct {
	TelegramBotToken string         `yaml:"telegram_bot_token"`
	GeminiAPIKey     string         `yaml:"gemini_api_key"`
	BotName          string         `yaml:"bot_name"`
	DNDTools         *DBConfig      `yaml:"dnd_tools"`
	SRD              *DBConfig      `yaml:"srd"`
	FoundryVTT       *FoundryConfig `yaml:"foundry_vtt"`
	Google           *GoogleConfig  `yaml:"google"`
}

const (
	configFilePath = "config.yaml"
)

// LoadFromFile loads the configuration from config.yaml file
func LoadFromFile() (*Config, error) {
	data, err := os.ReadFile(configFilePath)
	if err != nil {
		return nil, err
	}

	var config Config
	err = yaml.Unmarshal(data, &config)
	if err != nil {
		return nil, err
	}

	return &config, nil
}
