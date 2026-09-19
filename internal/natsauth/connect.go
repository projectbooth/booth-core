package natsauth

import (
	"fmt"
	"sync"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nkeys"
)

// ConnectOption returns a nats.Option that authenticates as name with the given grants.
// A fresh, short-lived credential is minted on every connect and reconnect, so a
// long-running process never presents an expired one and no static seed is held for
// longer than one connection attempt. Core uses this for its own bus connection.
func (a *Authority) ConnectOption(name string, g Grants) nats.Option {
	var (
		mu   sync.Mutex
		seed string
	)
	return nats.UserJWT(
		func() (string, error) {
			cred, err := a.MintUser(name, g, time.Hour)
			if err != nil {
				return "", err
			}
			mu.Lock()
			seed = cred.Seed
			mu.Unlock()
			return cred.JWT, nil
		},
		func(nonce []byte) ([]byte, error) {
			mu.Lock()
			s := seed
			mu.Unlock()
			kp, err := nkeys.FromSeed([]byte(s))
			if err != nil {
				return nil, fmt.Errorf("loading user seed to sign the server nonce: %w", err)
			}
			defer kp.Wipe()
			return kp.Sign(nonce)
		},
	)
}
