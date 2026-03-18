package cliproxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"

	codexauth "github.com/router-for-me/CLIProxyAPI/v6/internal/auth/codex"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/codexquota"
	"github.com/router-for-me/CLIProxyAPI/v6/internal/registry"
	coreauth "github.com/router-for-me/CLIProxyAPI/v6/sdk/cliproxy/auth"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

const (
	codexQuotaProbeURL       = "https://chatgpt.com/backend-api/wham/usage"
	codexQuotaProbeUserAgent = "codex_cli_rs/0.76.0 (Debian 13.0.0; x86_64) WindowsTerminal"
)

type codexQuotaMonitorConfig struct {
	enabled     bool
	interval    time.Duration
	concurrency int
	timeout     time.Duration
}

func (s *Service) codexQuotaConfig() codexQuotaMonitorConfig {
	s.cfgMu.RLock()
	defer s.cfgMu.RUnlock()

	cfg := codexQuotaMonitorConfig{
		enabled:     false,
		interval:    3 * time.Minute,
		concurrency: 2,
		timeout:     20 * time.Second,
	}
	if s == nil || s.cfg == nil {
		return cfg
	}
	monitor := s.cfg.CodexQuotaPredictiveRouting
	cfg.enabled = monitor.Enable
	if monitor.IntervalSeconds > 0 {
		cfg.interval = time.Duration(monitor.IntervalSeconds) * time.Second
	}
	if monitor.Concurrency > 0 {
		cfg.concurrency = monitor.Concurrency
	}
	if monitor.TimeoutSeconds > 0 {
		cfg.timeout = time.Duration(monitor.TimeoutSeconds) * time.Second
	}
	return cfg
}

func (s *Service) restartCodexQuotaMonitor() {
	if s == nil || s.coreManager == nil {
		return
	}

	cfg := s.codexQuotaConfig()

	s.codexQuotaMonitorMu.Lock()
	if s.codexQuotaMonitorCancel != nil {
		s.codexQuotaMonitorCancel()
		s.codexQuotaMonitorCancel = nil
	}
	if !cfg.enabled {
		s.codexQuotaMonitorMu.Unlock()
		return
	}
	ctx, cancel := context.WithCancel(context.Background())
	s.codexQuotaMonitorCancel = cancel
	s.codexQuotaMonitorMu.Unlock()

	go s.runCodexQuotaMonitor(ctx, cfg)
}

func (s *Service) stopCodexQuotaMonitor() {
	if s == nil {
		return
	}
	s.codexQuotaMonitorMu.Lock()
	defer s.codexQuotaMonitorMu.Unlock()
	if s.codexQuotaMonitorCancel != nil {
		s.codexQuotaMonitorCancel()
		s.codexQuotaMonitorCancel = nil
	}
}

func (s *Service) runCodexQuotaMonitor(ctx context.Context, cfg codexQuotaMonitorConfig) {
	if cfg.interval <= 0 {
		cfg.interval = 3 * time.Minute
	}
	if cfg.concurrency <= 0 {
		cfg.concurrency = 2
	}
	if cfg.timeout <= 0 {
		cfg.timeout = 20 * time.Second
	}

	log.Infof("codex proactive quota monitor started (interval=%s, concurrency=%d, timeout=%s)", cfg.interval, cfg.concurrency, cfg.timeout)
	ticker := time.NewTicker(cfg.interval)
	defer ticker.Stop()

	s.checkCodexQuotaOnce(ctx, cfg)
	for {
		select {
		case <-ctx.Done():
			log.Debug("codex proactive quota monitor stopped")
			return
		case <-ticker.C:
			s.checkCodexQuotaOnce(ctx, cfg)
		}
	}
}

func (s *Service) checkCodexQuotaOnce(ctx context.Context, cfg codexQuotaMonitorConfig) {
	if s == nil || s.coreManager == nil {
		return
	}
	targets := s.listCodexQuotaTargets()
	if len(targets) == 0 {
		return
	}

	sem := make(chan struct{}, cfg.concurrency)
	var wg sync.WaitGroup
	for _, auth := range targets {
		auth := auth
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				return
			}
			s.probeCodexAuthQuota(ctx, auth, cfg.timeout)
		}()
	}
	wg.Wait()
}

func (s *Service) listCodexQuotaTargets() []*coreauth.Auth {
	if s == nil || s.coreManager == nil {
		return nil
	}
	all := s.coreManager.List()
	targets := make([]*coreauth.Auth, 0, len(all))
	for _, auth := range all {
		if auth == nil || auth.Disabled {
			continue
		}
		if !strings.EqualFold(strings.TrimSpace(auth.Provider), "codex") {
			continue
		}
		accountType, _ := auth.AccountInfo()
		if !strings.EqualFold(accountType, "oauth") {
			continue
		}
		if codexQuotaAccountID(auth) == "" {
			continue
		}
		if len(codexQuotaModelIDs(auth.ID)) == 0 {
			continue
		}
		targets = append(targets, auth)
	}
	return targets
}

func (s *Service) probeCodexAuthQuota(ctx context.Context, auth *coreauth.Auth, timeout time.Duration) {
	if s == nil || s.coreManager == nil || auth == nil {
		return
	}
	modelIDs := codexQuotaModelIDs(auth.ID)
	if len(modelIDs) == 0 {
		return
	}

	accountID := codexQuotaAccountID(auth)
	if accountID == "" {
		return
	}

	statusCode, body, latestAuth, err := s.fetchCodexQuota(ctx, auth, accountID, timeout)
	if err != nil {
		log.Debugf("codex proactive quota probe failed for %s: %v", auth.ID, err)
		return
	}
	if latestAuth != nil {
		auth = latestAuth
	}

	now := time.Now()
	switch statusCode {
	case http.StatusOK:
		snapshot := codexquota.ParseUsage(body, now)
		if !snapshot.FiveHour.Present && !snapshot.Weekly.Present {
			return
		}
		if recoverAt, label, exhausted := codexQuotaRecovery(snapshot); exhausted {
			if recoverAt.IsZero() {
				return
			}
			s.applyCodexQuotaCooldown(ctx, auth.ID, modelIDs, recoverAt, label)
			return
		}
		s.clearCodexQuotaCooldown(ctx, auth.ID, modelIDs)
	case http.StatusTooManyRequests:
		retryAfter := parseCodexQuotaRetryAfter(body, now)
		if retryAfter == nil || *retryAfter <= 0 {
			return
		}
		s.applyCodexQuotaCooldown(ctx, auth.ID, modelIDs, now.Add(*retryAfter), "upstream")
	}
}

func (s *Service) fetchCodexQuota(ctx context.Context, auth *coreauth.Auth, accountID string, timeout time.Duration) (int, []byte, *coreauth.Auth, error) {
	if s == nil || s.coreManager == nil {
		return 0, nil, auth, fmt.Errorf("codex quota monitor unavailable")
	}
	exec, ok := s.coreManager.Executor("codex")
	if !ok || exec == nil {
		return 0, nil, auth, fmt.Errorf("codex executor unavailable")
	}

	current := auth.Clone()
	if current == nil {
		current = auth
	}
	for attempt := 0; attempt < 2; attempt++ {
		reqCtx, cancel := context.WithTimeout(ctx, timeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, codexQuotaProbeURL, nil)
		if err != nil {
			cancel()
			return 0, nil, current, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("User-Agent", codexQuotaProbeUserAgent)
		req.Header.Set("Chatgpt-Account-Id", accountID)

		resp, err := exec.HttpRequest(reqCtx, current, req)
		if err != nil {
			cancel()
			return 0, nil, current, err
		}

		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		cancel()
		if readErr != nil {
			return 0, nil, current, readErr
		}
		if resp.StatusCode == http.StatusUnauthorized && attempt == 0 {
			refreshed, refreshErr := s.refreshCodexProbeAuth(ctx, current)
			if refreshErr != nil || refreshed == nil {
				if refreshErr != nil {
					return resp.StatusCode, body, current, refreshErr
				}
				return resp.StatusCode, body, current, nil
			}
			current = refreshed
			if nextAccountID := codexQuotaAccountID(current); nextAccountID != "" {
				accountID = nextAccountID
			}
			continue
		}
		return resp.StatusCode, body, current, nil
	}

	return 0, nil, current, fmt.Errorf("codex quota probe exhausted refresh retries")
}

func (s *Service) refreshCodexProbeAuth(ctx context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	if s == nil || s.coreManager == nil || auth == nil {
		return auth, nil
	}
	exec, ok := s.coreManager.Executor("codex")
	if !ok || exec == nil {
		return auth, nil
	}

	updated, err := exec.Refresh(ctx, auth.Clone())
	if err != nil {
		return nil, err
	}
	if updated == nil {
		return auth, nil
	}

	now := time.Now()
	updated.LastRefreshedAt = now
	updated.NextRefreshAfter = time.Time{}
	updated.LastError = nil
	updated.UpdatedAt = now
	saved, err := s.coreManager.Update(ctx, updated)
	if err != nil {
		return nil, err
	}
	return saved, nil
}

func (s *Service) applyCodexQuotaCooldown(ctx context.Context, authID string, modelIDs []string, recoverAt time.Time, label string) {
	if s == nil || s.coreManager == nil || authID == "" || len(modelIDs) == 0 || recoverAt.IsZero() {
		return
	}
	if !s.codexQuotaNeedsApply(authID, modelIDs, recoverAt) {
		return
	}

	retryAfter := time.Until(recoverAt)
	if retryAfter <= 0 {
		return
	}
	msg := "codex proactive quota cooldown"
	if label != "" {
		msg = "codex proactive " + label + " quota cooldown"
	}

	resultCtx := coreauth.WithSkipPersist(ctx)
	for _, modelID := range modelIDs {
		retryCopy := retryAfter
		s.coreManager.MarkResult(resultCtx, coreauth.Result{
			AuthID:    authID,
			Provider:  "codex",
			Model:     modelID,
			Success:   false,
			RetryAfter: &retryCopy,
			Error: &coreauth.Error{
				Code:       "quota",
				Message:    msg,
				Retryable:  true,
				HTTPStatus: http.StatusTooManyRequests,
			},
		})
	}
	log.Infof("codex proactive quota cooldown applied to %s until %s (%s)", authID, recoverAt.Format(time.RFC3339), label)
}

func (s *Service) clearCodexQuotaCooldown(ctx context.Context, authID string, modelIDs []string) {
	if s == nil || s.coreManager == nil || authID == "" || len(modelIDs) == 0 {
		return
	}
	if !s.codexQuotaNeedsClear(authID, modelIDs) {
		return
	}

	resultCtx := coreauth.WithSkipPersist(ctx)
	for _, modelID := range modelIDs {
		s.coreManager.MarkResult(resultCtx, coreauth.Result{
			AuthID:   authID,
			Provider: "codex",
			Model:    modelID,
			Success:  true,
		})
	}
	log.Infof("codex proactive quota cooldown cleared for %s", authID)
}

func (s *Service) codexQuotaNeedsApply(authID string, modelIDs []string, recoverAt time.Time) bool {
	if s == nil || s.coreManager == nil {
		return false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil {
		return true
	}
	for _, modelID := range modelIDs {
		state := auth.ModelStates[modelID]
		if state == nil || !state.Quota.Exceeded {
			return true
		}
		if state.Quota.NextRecoverAt.Before(recoverAt.Add(-time.Second)) {
			return true
		}
	}
	return false
}

func (s *Service) codexQuotaNeedsClear(authID string, modelIDs []string) bool {
	if s == nil || s.coreManager == nil {
		return false
	}
	auth, ok := s.coreManager.GetByID(authID)
	if !ok || auth == nil {
		return false
	}
	for _, modelID := range modelIDs {
		state := auth.ModelStates[modelID]
		if state != nil && state.Quota.Exceeded {
			return true
		}
	}
	return false
}

func codexQuotaModelIDs(authID string) []string {
	models := registry.GetGlobalRegistry().GetModelsForClient(authID)
	if len(models) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(models))
	ids := make([]string, 0, len(models))
	for _, model := range models {
		if model == nil {
			continue
		}
		id := strings.TrimSpace(model.ID)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		ids = append(ids, id)
	}
	return ids
}

func codexQuotaAccountID(auth *coreauth.Auth) string {
	if auth == nil {
		return ""
	}
	if auth.Metadata != nil {
		if raw, ok := auth.Metadata["account_id"].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
		if raw, ok := auth.Metadata["chatgpt_account_id"].(string); ok {
			if value := strings.TrimSpace(raw); value != "" {
				return value
			}
		}
		if idToken, ok := auth.Metadata["id_token"].(string); ok {
			claims, err := codexauth.ParseJWTToken(strings.TrimSpace(idToken))
			if err == nil && claims != nil {
				if value := strings.TrimSpace(claims.GetAccountID()); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func codexQuotaRecovery(snapshot codexquota.Snapshot) (time.Time, string, bool) {
	switch {
	case snapshot.Weekly.Present && snapshot.Weekly.Exhausted && (!snapshot.FiveHour.Exhausted || snapshot.Weekly.RecoverAt.After(snapshot.FiveHour.RecoverAt) || snapshot.FiveHour.RecoverAt.IsZero()):
		return snapshot.Weekly.RecoverAt, "weekly", true
	case snapshot.FiveHour.Present && snapshot.FiveHour.Exhausted:
		return snapshot.FiveHour.RecoverAt, "5h", true
	default:
		return time.Time{}, "", false
	}
}

func parseCodexQuotaRetryAfter(body []byte, now time.Time) *time.Duration {
	if strings.TrimSpace(gjson.GetBytes(body, "error.type").String()) != "usage_limit_reached" {
		return nil
	}
	if resetsAt := gjson.GetBytes(body, "error.resets_at").Int(); resetsAt > 0 {
		resetAtTime := time.Unix(resetsAt, 0)
		if resetAtTime.After(now) {
			retryAfter := resetAtTime.Sub(now)
			return &retryAfter
		}
	}
	if resetsInSeconds := gjson.GetBytes(body, "error.resets_in_seconds").Int(); resetsInSeconds > 0 {
		retryAfter := time.Duration(resetsInSeconds) * time.Second
		return &retryAfter
	}
	return nil
}
