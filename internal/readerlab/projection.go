package readerlab

import (
	"context"
	"database/sql"
	"fmt"

	"github.com/sysop/ultrabridge/internal/readercontract"
	"github.com/sysop/ultrabridge/internal/readerstore"
)

// ProjectFixture explicitly drains a disposable fixture, then reads the actual
// UB reducer. This is a CLI test action, NOT an HTTP endpoint or production worker.
// Fixed sweep/page budgets fail rather than spin on missing dependencies.
func ProjectFixture(ctx context.Context, db *sql.DB, id string) (*readercontract.AnnotationProjection, error) {
	if err := readerstore.Install(ctx, db); err != nil {
		return nil, err
	}
	s := readerstore.New(db)
	for sweep := 0; sweep < 16; sweep++ {
		var after int64
		settled := 0
		for page := 0; ; page++ {
			if page == 2048 {
				return nil, fmt.Errorf("fixture drain page budget exceeded")
			}
			result, err := s.Drain(ctx, after, 128)
			if err != nil {
				return nil, err
			}
			for _, r := range result.Records {
				if r.State != "pending" {
					settled++
				}
			}
			after = result.Next
			if after == 0 {
				break
			}
		}
		if settled == 0 {
			return s.Projection(ctx, id, readerstore.DefaultLimits())
		}
	}
	return nil, fmt.Errorf("fixture drain sweep budget exceeded")
}
