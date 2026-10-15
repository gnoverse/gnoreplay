package main

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"github.com/hetznercloud/hcloud-go/v2/hcloud"
)

// Machine is a worker machine the provisioner created.
type Machine struct {
	ID      string
	Name    string
	Created time.Time // as reported by the provider: billing hours count from here
	Labels  map[string]string
}

// Provisioner creates, reinstalls and deletes the machines replays run on.
type Provisioner interface {
	// Create boots a machine running userData (a shell script) at startup.
	Create(ctx context.Context, name, userData string, labels map[string]string) (Machine, error)
	// Rebuild reinstalls a machine from its image, wiping its disk, and boots
	// it running userData: nothing that ran on it before survives.
	Rebuild(ctx context.Context, id, userData string) error
	// SetLabels replaces a machine's labels.
	SetLabels(ctx context.Context, id string, labels map[string]string) error
	// Delete deletes a machine; deleting one that is already gone succeeds.
	Delete(ctx context.Context, id string) error
	// List returns the worker machines that exist.
	List(ctx context.Context) ([]Machine, error)
}

// Worker machines' labels, which deploy/watchdog.sh reads too.
const (
	labelRole  = "gnoreplay" // "worker" on every worker machine
	labelJob   = "gnoreplay-job"
	labelSince = "gnoreplay-since"
)

// workerLabels are the labels of a worker machine running job, or idle (job
// 0), since then (Unix seconds).
func workerLabels(job int64, since time.Time) map[string]string {
	labels := map[string]string{labelRole: "worker", labelSince: strconv.FormatInt(since.Unix(), 10)}
	if job != 0 {
		labels[labelJob] = strconv.FormatInt(job, 10)
	}
	return labels
}

type hetznerProvisioner struct {
	c   *hcloud.Client
	cfg *HetznerConfig
}

func newHetznerProvisioner(token string, cfg *HetznerConfig) *hetznerProvisioner {
	return &hetznerProvisioner{c: hcloud.NewClient(hcloud.WithToken(token), hcloud.WithApplication("gnoreplay", "")), cfg: cfg}
}

func (p *hetznerProvisioner) Create(ctx context.Context, name, userData string, labels map[string]string) (Machine, error) {
	keys := make([]*hcloud.SSHKey, len(p.cfg.SSHKeys))
	for i, id := range p.cfg.SSHKeys {
		keys[i] = &hcloud.SSHKey{ID: id}
	}
	res, _, err := p.c.Server.Create(ctx, hcloud.ServerCreateOpts{
		Name:       name,
		ServerType: &hcloud.ServerType{Name: p.cfg.ServerType},
		Image:      &hcloud.Image{Name: p.cfg.Image},
		Location:   &hcloud.Location{Name: p.cfg.Location},
		SSHKeys:    keys,
		UserData:   userData,
		Labels:     labels,
		Networks:   []*hcloud.Network{{ID: p.cfg.Network}},
	})
	if err != nil {
		return Machine{}, err
	}
	return toMachine(res.Server), nil
}

func toMachine(s *hcloud.Server) Machine {
	return Machine{ID: strconv.FormatInt(s.ID, 10), Name: s.Name, Created: s.Created, Labels: s.Labels}
}

func serverRef(id string) (*hcloud.Server, error) {
	n, err := strconv.ParseInt(id, 10, 64)
	if err != nil {
		return nil, err
	}
	return &hcloud.Server{ID: n}, nil
}

func (p *hetznerProvisioner) Rebuild(ctx context.Context, id, userData string) error {
	server, err := serverRef(id)
	if err != nil {
		return err
	}
	res, _, err := p.c.Server.RebuildWithResult(ctx, server, hcloud.ServerRebuildOpts{
		Image:    &hcloud.Image{Name: p.cfg.Image},
		UserData: &userData,
	})
	if err != nil {
		return err
	}
	if err := p.c.Action.WaitFor(ctx, res.Action); err != nil {
		return err
	}
	// Should a machine that was off (workers power themselves off at their
	// max age) stay off once rebuilt, start it.
	s, _, err := p.c.Server.GetByID(ctx, server.ID)
	if err != nil {
		return err
	}
	if s == nil {
		return fmt.Errorf("server %s is gone", id)
	}
	if s.Status == hcloud.ServerStatusOff {
		action, _, err := p.c.Server.Poweron(ctx, s)
		if err != nil {
			return err
		}
		return p.c.Action.WaitFor(ctx, action)
	}
	return nil
}

func (p *hetznerProvisioner) SetLabels(ctx context.Context, id string, labels map[string]string) error {
	server, err := serverRef(id)
	if err != nil {
		return err
	}
	_, _, err = p.c.Server.Update(ctx, server, hcloud.ServerUpdateOpts{Labels: labels})
	return err
}

func (p *hetznerProvisioner) Delete(ctx context.Context, id string) error {
	server, err := serverRef(id)
	if err != nil {
		return err
	}
	_, _, err = p.c.Server.DeleteWithResult(ctx, server)
	if hcloud.IsError(err, hcloud.ErrorCodeNotFound) {
		return nil
	}
	return err
}

func (p *hetznerProvisioner) List(ctx context.Context) ([]Machine, error) {
	servers, err := p.c.Server.AllWithOpts(ctx, hcloud.ServerListOpts{
		ListOpts: hcloud.ListOpts{LabelSelector: labelRole + "=worker"},
	})
	if err != nil {
		return nil, err
	}
	out := make([]Machine, len(servers))
	for i, s := range servers {
		out[i] = toMachine(s)
	}
	return out, nil
}
