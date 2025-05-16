package main

import (
	cfg "github.com/conductorone/baton-grafana/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/config"
)

func main() {
	config.Generate("grafana", cfg.Config)
}
