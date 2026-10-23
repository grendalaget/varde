package main

import (
	"context"
	"log"
	"net"

	pion "github.com/pion/webrtc/v4"

	"sfu-v2/embed"
	"sfu-v2/internal/config"
)

var Version = "dev"

func main() {
	embed.Version = Version
	if err := embed.Run(context.Background()); err != nil {
		log.Fatal(err)
	}
}

func newSettingEngine(cfg *config.Config) (pion.SettingEngine, error) {
	return embed.NewSettingEngine(cfg)
}

func registerCodecs(me *pion.MediaEngine) error {
	return embed.RegisterCodecs(me)
}

func shouldBindICEUDPAddress(ip net.IP) bool {
	return embed.ShouldBindICEUDPAddress(ip)
}
