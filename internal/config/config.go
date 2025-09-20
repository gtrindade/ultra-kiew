package config

import (
	"os"

	"gopkg.in/yaml.v2"
)

type DBConfig struct {
	Host     string `yaml:"host"`
	Port     string `yaml:"port"`
	User     string `yaml:"user"`
	Password string `yaml:"password"`
	Name     string `yaml:"name"`
}

type Config struct {
	BotName  string    `yaml:"bot_name"`
	DNDTools *DBConfig `yaml:"dnd_tools"`
	SRD      *DBConfig `yaml:"srd"`
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
