package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/digitalocean/godo"
)

// Machine is a worker machine the provisioner created.
type Machine struct {
	ID      string
	Name    string
	Created time.Time // as reported by the provider
}

// Provisioner creates and deletes the disposable machines replays run on.
type Provisioner interface {
	// Create boots a machine running userData (a shell script) at startup.
	Create(ctx context.Context, name, userData string) (Machine, error)
	// Delete deletes a machine; deleting one that is already gone succeeds.
	Delete(ctx context.Context, id string) error
	// List returns the worker machines that exist.
	List(ctx context.Context) ([]Machine, error)
}

type doProvisioner struct {
	c   *godo.Client
	cfg *DigitalOceanConfig
}

func newDOProvisioner(token string, cfg *DigitalOceanConfig) *doProvisioner {
	return &doProvisioner{c: godo.NewFromToken(token), cfg: cfg}
}

func (p *doProvisioner) Create(ctx context.Context, name, userData string) (Machine, error) {
	keys := make([]godo.DropletCreateSSHKey, len(p.cfg.SSHKeys))
	for i, k := range p.cfg.SSHKeys {
		if id, err := strconv.Atoi(k); err == nil {
			keys[i] = godo.DropletCreateSSHKey{ID: id}
		} else {
			keys[i] = godo.DropletCreateSSHKey{Fingerprint: k}
		}
	}
	d, _, err := p.c.Droplets.Create(ctx, &godo.DropletCreateRequest{
		Name:     name,
		Region:   p.cfg.Region,
		Size:     p.cfg.Size,
		Image:    godo.DropletCreateImage{Slug: p.cfg.Image},
		SSHKeys:  keys,
		UserData: userData,
		Tags:     []string{p.cfg.Tag},
		VPCUUID:  p.cfg.VPCUUID,
	})
	if err != nil {
		return Machine{}, err
	}
	return toMachine(d)
}

func toMachine(d *godo.Droplet) (Machine, error) {
	created, err := time.Parse(time.RFC3339, d.Created)
	if err != nil {
		return Machine{}, fmt.Errorf("droplet %d: created_at %q: %w", d.ID, d.Created, err)
	}
	return Machine{ID: strconv.Itoa(d.ID), Name: d.Name, Created: created}, nil
}

func (p *doProvisioner) Delete(ctx context.Context, id string) error {
	n, err := strconv.Atoi(id)
	if err != nil {
		return err
	}
	res, err := p.c.Droplets.Delete(ctx, n)
	var errRes *godo.ErrorResponse
	if errors.As(err, &errRes) && res != nil && res.StatusCode == http.StatusNotFound {
		return nil
	}
	return err
}

func (p *doProvisioner) List(ctx context.Context) ([]Machine, error) {
	var out []Machine
	opt := &godo.ListOptions{PerPage: 200}
	for {
		droplets, res, err := p.c.Droplets.ListByTag(ctx, p.cfg.Tag, opt)
		if err != nil {
			return nil, err
		}
		for i := range droplets {
			m, err := toMachine(&droplets[i])
			if err != nil {
				return nil, err
			}
			out = append(out, m)
		}
		if res.Links == nil || res.Links.IsLastPage() {
			return out, nil
		}
		page, err := res.Links.CurrentPage()
		if err != nil {
			return nil, err
		}
		opt.Page = page + 1
	}
}
