package app

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"

	"github.com/go-tangra/go-tangra-lcm/sdk/v4/pkg/lcmidentity"
	freya "github.com/go-tangra/go-tangra/v4"
)

// enrollIdentity obtains the mesh SVID by enrolling with lcm over the
// network (enroll.enabled with identity.provider: provided): the key is
// generated locally, a single-use join token authorizes the first enrolment
// and the provider renews over mTLS afterwards (persisted in the state file).
func (a *App) enrollIdentity(ctx context.Context) ([]freya.Option, error) {
	e := a.Cfg.Enroll
	if !e.Enabled {
		return nil, nil
	}
	raw, err := os.ReadFile(e.TokenFile) // #nosec G304 -- operator-supplied token path
	if err != nil {
		return nil, errors.New("app: enroll token unreadable")
	}
	prov, err := lcmidentity.NewNet(ctx, lcmidentity.NetConfig{EnrollURL: e.EnrollURL, LCMGRPCTarget: e.LCMGRPCTarget, TenantID: e.TenantID,
		TrustDomain: a.Cfg.TrustDomain, ServiceName: a.Cfg.ServiceName, EnrollmentToken: strings.TrimSpace(string(raw)), Insecure: e.Insecure,
		StateFile: e.StateFile})
	if err != nil {
		return nil, fmt.Errorf("app: mesh enrollment: %w", err)
	}
	a.closers = append(a.closers, func() { _ = prov.Close() })
	return []freya.Option{freya.WithIdentityProvider(prov)}, nil
}
