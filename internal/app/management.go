package app

import (
	"net/http"

	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/httpapi"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/metrics"
	"github.com/go-tangra/go-tangra-sms-gw/v4/internal/webhook"
	"github.com/go-tangra/go-tangra-sms-gw/v4/pkg/smsgwmanifest"
)

// buildManagement mounts the V4 management API (and the federated remote
// when one is embedded) on the mesh HTTP mux.
func (a *App) buildManagement(o Options) error {
	dash, err := metrics.NewDashboard(a.Cfg.Monitoring.PrometheusURL)
	if err != nil {
		return err
	}
	a.API, err = httpapi.New(httpapi.Config{Store: a.Repo, Authz: a.Authz, Envelope: a.Envelope, SMS: a.SMS, Senders: a.Senders, Dashboard: dash,
		Audit: a.Audit, Webhook: webhook.Policy{AllowHTTP: a.Cfg.Webhook.AllowHTTP, AllowPrivate: a.Cfg.Webhook.AllowPrivate},
		Log: a.Log.With("component", "management")})
	if err != nil {
		return err
	}
	a.Management.Handle(smsgwmanifest.APIPrefix+"/", a.API)
	if o.Remote != nil {
		a.Management.Handle("GET "+smsgwmanifest.RemotePrefix+"/", http.StripPrefix(smsgwmanifest.RemotePrefix, httpapi.RemoteHandler(o.Remote)))
	}
	return nil
}
