package tools

import (
	"time"

	alohaclawtools "github.com/sipeed/picoclaw/pkg/tools/alohaclaw"
)

type AlohaClawTool = alohaclawtools.AlohaClawTool

func NewAlohaClawTool(brokerIP string, port int, botID, botPassword string, replyTimeout time.Duration) *AlohaClawTool {
	return alohaclawtools.NewAlohaClawTool(brokerIP, port, botID, botPassword, replyTimeout)
}
