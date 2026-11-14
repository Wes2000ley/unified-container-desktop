package helper

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
)

// Config describes the Linux helper's listener, managed data, and TLS files.
type Config struct {
	Listen     string `json:"listen"`
	DataRoot   string `json:"dataRoot"`
	CACert     string `json:"caCert"`
	ServerCert string `json:"serverCert"`
	ServerKey  string `json:"serverKey"`
}

// LoadConfig loads one JSON configuration, applying the standard Linux paths
// and loopback listener to omitted fields.
func LoadConfig(path string) (Config, error) {
	config := Config{
		Listen:     "127.0.0.1:7443",
		DataRoot:   "/var/lib/ucd",
		CACert:     "/etc/ucd/ca.pem",
		ServerCert: "/etc/ucd/server.pem",
		ServerKey:  "/etc/ucd/server-key.pem",
	}
	file, err := os.Open(path)
	if err != nil {
		return Config{}, fmt.Errorf("open helper configuration: %w", err)
	}
	defer file.Close()
	data, err := io.ReadAll(io.LimitReader(file, (1<<20)+1))
	if err != nil {
		return Config{}, fmt.Errorf("read helper configuration: %w", err)
	}
	if len(data) > 1<<20 {
		return Config{}, errors.New("helper configuration exceeds 1 MiB")
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&config); err != nil {
		return Config{}, fmt.Errorf("decode helper configuration: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return Config{}, errors.New("helper configuration must contain one JSON object")
	}
	if err := config.validate(); err != nil {
		return Config{}, err
	}
	return config, nil
}

func (c Config) validate() error {
	host, port, err := net.SplitHostPort(c.Listen)
	if err != nil || port == "" {
		return errors.New("helper listen must be a loopback host and port")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return errors.New("helper must listen on a loopback address")
	}
	if !filepath.IsAbs(c.DataRoot) || !filepath.IsAbs(c.CACert) || !filepath.IsAbs(c.ServerCert) || !filepath.IsAbs(c.ServerKey) {
		return errors.New("helper data root and TLS paths must be absolute")
	}
	return nil
}
