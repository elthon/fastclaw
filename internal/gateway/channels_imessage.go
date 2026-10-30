package gateway

import (
	"github.com/fastclaw-ai/fastclaw/internal/bus"
	"github.com/fastclaw-ai/fastclaw/internal/channels"
	"github.com/fastclaw-ai/fastclaw/internal/config"
)

// registerIMessageChannels starts the local (macOS) iMessage adapter.
// It polls chat.db, so it's registered as a singleton — a second
// replica tailing the same database would double-deliver.
func registerIMessageChannels(chCfg config.ChannelConfig, mb *bus.MessageBus, chanMgr *channels.Manager, hot bool) error {
	for accountID := range chCfg.Accounts {
		im, err := channels.NewIMessage(accountID, mb)
		if err != nil {
			return err
		}
		registerSingleton(chanMgr, im, hot)
	}
	return nil
}
