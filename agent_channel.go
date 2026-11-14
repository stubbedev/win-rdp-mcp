package main

import (
	"context"
	"errors"
	"fmt"
	"time"
)

const (
	agentRetryInterval  = 500 * time.Millisecond
	agentOrphanTimeout  = 30 * time.Minute
	agentReconnectPause = 500 * time.Millisecond
)

func runAgentChannel(ctx context.Context, build string) error {
	resolved, err := resolveSchemas()
	if err != nil {
		return err
	}
	orphanSince := time.Now()
	lastErr := ""
	for {
		ch, err := openAgentChannel(agentChannelName)
		if err != nil {
			if time.Since(orphanSince) > agentOrphanTimeout {
				return fmt.Errorf("no controller reachable for %s, giving up: %w", agentOrphanTimeout, err)
			}
			if msg := err.Error(); msg != lastErr {
				logf("waiting for the controller: %v", err)
				lastErr = msg
			}
			if !sleepCtx(ctx, agentRetryInterval) {
				return nil
			}
			continue
		}
		logf("connected to the controller over the %s channel", agentChannelName)
		lastErr = ""
		err = serveRPC(ctx, ch, build, resolved)
		closeErr := ch.Close()
		switch {
		case errors.Is(err, errAgentRetired):
			logf("%v; exiting", err)
			return nil
		case ctx.Err() != nil:
			return nil
		}
		logf("controller channel ended (%v); reconnecting", errors.Join(err, closeErr))
		orphanSince = time.Now()
		if !sleepCtx(ctx, agentReconnectPause) {
			return nil
		}
	}
}
