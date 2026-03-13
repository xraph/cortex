package dashboard

import (
	"context"
	"time"

	shieldengine "github.com/xraph/shield/engine"
	"github.com/xraph/shield/profile"
	"github.com/xraph/shield/scan"
)

// shieldAdapter wraps a *shield/engine.Engine to satisfy the SafetySource interface.
type shieldAdapter struct {
	eng *shieldengine.Engine
}

// NewShieldSafetySource creates a SafetySource backed by a shield engine.
func NewShieldSafetySource(eng *shieldengine.Engine) SafetySource {
	return &shieldAdapter{eng: eng}
}

func (a *shieldAdapter) ListProfiles(ctx context.Context) ([]SafetyProfileInfo, error) {
	s := a.eng.Store()
	if s == nil {
		return nil, nil
	}
	profiles, err := s.ListProfiles(ctx, &profile.ListFilter{})
	if err != nil {
		return nil, err
	}
	result := make([]SafetyProfileInfo, 0, len(profiles))
	for _, p := range profiles {
		result = append(result, SafetyProfileInfo{
			ID:          p.ID.String(),
			Name:        p.Name,
			Description: p.Description,
			Enabled:     p.Enabled,
		})
	}
	return result, nil
}

func (a *shieldAdapter) GetScanSummary(ctx context.Context, _ string) (*ScanSummary, error) {
	s := a.eng.Store()
	if s == nil {
		return &ScanSummary{}, nil
	}
	stats, err := s.ScanStats(ctx, &scan.StatsFilter{})
	if err != nil {
		return &ScanSummary{}, nil
	}
	var blockRate float64
	if stats.TotalScans > 0 {
		blockRate = float64(stats.BlockedCount) / float64(stats.TotalScans) * 100
	}
	return &ScanSummary{
		TotalScans:   stats.TotalScans,
		BlockedScans: stats.BlockedCount,
		FlaggedScans: stats.FlaggedCount,
		AllowedScans: stats.AllowedCount,
		BlockRate:    blockRate,
	}, nil
}

func (a *shieldAdapter) GetRunScans(ctx context.Context, runID string) ([]ScanInfo, error) {
	s := a.eng.Store()
	if s == nil {
		return nil, nil
	}
	// List recent scans and filter by metadata run_id.
	scans, err := s.ListScans(ctx, &scan.ListFilter{Limit: 100})
	if err != nil {
		return nil, err
	}
	var result []ScanInfo
	for _, sc := range scans {
		if rid, ok := sc.Metadata["run_id"].(string); ok && rid == runID {
			result = append(result, ScanInfo{
				ID:           sc.ID.String(),
				Direction:    string(sc.Direction),
				Decision:     string(sc.Decision),
				Blocked:      sc.Blocked,
				FindingCount: len(sc.Findings),
				PIICount:     sc.PIICount,
				ProfileUsed:  sc.ProfileUsed,
				DurationMs:   sc.Duration.Milliseconds(),
				CreatedAt:    sc.CreatedAt.Format(time.RFC3339),
			})
		}
	}
	return result, nil
}
