// Package docker is a discovery.Source for Docker containers.
package docker

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/moby/moby/api/types/container"
	"github.com/moby/moby/api/types/events"
	"github.com/moby/moby/api/types/network"
	"github.com/moby/moby/client"
	"go.uber.org/zap"

	"github.com/perryprojects/docker-swagger-aggregator/internal/discovery"
)

// SourceName is the value of discovery.Container.Source for Docker containers.
const SourceName = "docker"

const (
	composeServiceLabel = "com.docker.compose.service"
	composeNumberLabel  = "com.docker.compose.container-number"
	composeProjectLabel = "com.docker.compose.project"
	maxBackoff          = 30 * time.Second
	stableAfter         = time.Minute
)

// watchedActions are the container events the daemon is asked for. "health_status" is matched by
// prefix daemon-side (the event is named "health_status: healthy"). handle checks again, so a
// daemon that ignores the filter only costs traffic.
var watchedActions = []string{"start", "die", "stop", "destroy", "health_status"}

// API is the part of the Docker client the source uses; *client.Client implements it.
type API interface {
	ContainerList(ctx context.Context, opts client.ContainerListOptions) (client.ContainerListResult, error)
	ContainerInspect(
		ctx context.Context,
		id string,
		opts client.ContainerInspectOptions,
	) (client.ContainerInspectResult, error)
	Events(ctx context.Context, opts client.EventsListOptions) client.EventsResult
}

// Config configures a Source; the zero value is usable.
type Config struct {
	// Network is the preferred network to reach containers on; empty = first network with an IP.
	Network string
	// LabelPrefix is the label namespace (default discovery.DefaultPrefix).
	LabelPrefix string
	// TraefikServerURL derives server-url from Traefik router labels when the
	// container has no explicit server-url label (see TraefikServerURL).
	TraefikServerURL bool
	// PreferPublishedPorts reaches a container through its published host port
	// (127.0.0.1:<port>) when it has one, instead of its network IP. For running
	// the API on the host with Docker Desktop, where container IPs are not routable.
	PreferPublishedPorts bool
	// ReconnectBackoff is the first delay before reconnecting (default 1s).
	ReconnectBackoff time.Duration
}

// Source lists labeled containers and follows their events.
type Source struct {
	api API
	cfg Config
	log *zap.Logger
}

// New creates a Source.
func New(api API, cfg Config, log *zap.Logger) *Source {
	if cfg.LabelPrefix == "" {
		cfg.LabelPrefix = discovery.DefaultPrefix
	}
	if cfg.ReconnectBackoff <= 0 {
		cfg.ReconnectBackoff = time.Second
	}
	return &Source{api: api, cfg: cfg, log: log}
}

// Run blocks until ctx is done. Each session resyncs (list) and then streams
// events; on any stream error it reconnects with exponential backoff, so a
// daemon restart is survived.
func (s *Source) Run(ctx context.Context, h discovery.Handler) {
	backoff := s.cfg.ReconnectBackoff
	timer := time.NewTimer(0)
	defer timer.Stop()
	for ctx.Err() == nil {
		start := time.Now()
		err := s.session(ctx, h)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > stableAfter {
			backoff = s.cfg.ReconnectBackoff // it worked for a while; start over
		}
		s.log.Warn("docker event stream ended, reconnecting", zap.Error(err), zap.Duration("in", backoff))
		timer.Reset(backoff)
		select {
		case <-ctx.Done():
			return
		case <-timer.C:
		}
		backoff = discovery.Backoff(backoff, maxBackoff)
	}
}

func (s *Source) enableFilter() string { return discovery.EnableKey(s.cfg.LabelPrefix) + "=true" }

func (s *Source) session(ctx context.Context, h discovery.Handler) error {
	ctx, cancel := context.WithCancel(ctx) // also tears down the event stream on early return
	defer cancel()

	// Subscribe before listing so nothing between the two is lost. Since is
	// second-granular, so a few events may replay; Apply is idempotent.
	ev := s.api.Events(ctx, client.EventsListOptions{
		Since:   strconv.FormatInt(time.Now().Unix(), 10),
		Filters: client.Filters{}.Add("type", "container").Add("label", s.enableFilter()).Add("event", watchedActions...),
	})

	list, err := s.api.ContainerList(ctx, client.ContainerListOptions{
		All:     true, // stopped containers show up as "down"
		Filters: client.Filters{}.Add("label", s.enableFilter()),
	})
	if err != nil {
		return fmt.Errorf("list containers: %w", err)
	}
	live := make(map[string]bool, len(list.Items))
	for _, c := range list.Items {
		live[c.ID] = true
		s.apply(ctx, h, c.ID)
	}
	h.Retain(live)
	s.log.Info("docker sync complete", zap.Int("containers", len(live)))

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case streamErr := <-ev.Err:
			if streamErr == nil {
				streamErr = errors.New("event stream closed")
			}
			return streamErr
		case m := <-ev.Messages:
			s.handle(ctx, h, m)
		}
	}
}

func (s *Source) handle(ctx context.Context, h discovery.Handler, m events.Message) {
	if m.Type != events.ContainerEventType {
		return
	}
	switch {
	case m.Action == events.ActionDestroy:
		h.Remove(m.Actor.ID)
	case m.Action == events.ActionStart, m.Action == events.ActionDie, m.Action == events.ActionStop,
		strings.HasPrefix(string(m.Action), string(events.ActionHealthStatus)):
		// Matched client-side on purpose: the daemon names this event
		// "health_status: healthy", so a prefix match is the safe choice.
		s.apply(ctx, h, m.Actor.ID)
	}
}

func (s *Source) apply(ctx context.Context, h discovery.Handler, id string) {
	res, err := s.api.ContainerInspect(ctx, id, client.ContainerInspectOptions{})
	if err != nil {
		s.log.Warn("inspect failed", zap.String("id", id), zap.Error(err))
		return
	}
	c, ok := s.toContainer(res)
	if !ok {
		s.log.Warn("skipping container", zap.String("id", id))
		return
	}
	h.Apply(ctx, c)
}

// toContainer resolves an inspect result. ok is false for containers that
// cannot be handled at all (incomplete response, not labeled); invalid labels
// or a missing network still yield a Container with Problem set.
func (s *Source) toContainer(res client.ContainerInspectResult) (discovery.Container, bool) {
	i := res.Container
	if i.Config == nil || i.State == nil {
		return discovery.Container{}, false
	}
	name := strings.TrimPrefix(i.Name, "/")
	t, err := discovery.ParseTarget(
		s.cfg.LabelPrefix,
		i.Config.Labels,
		[]string{i.Config.Labels[composeServiceLabel], name},
		exposedTCPPorts(i.Config),
	)
	if errors.Is(err, discovery.ErrNotEnabled) {
		return discovery.Container{}, false
	}
	if err != nil {
		// Invalid labels: show the service as down with the reason, under the name it would have had.
		return discovery.Container{
			ID:      i.ID,
			Name:    s.displayName(i.Config.Labels, name),
			Source:  SourceName,
			Running: i.State.Running,
			Problem: err.Error(),
		}, true
	}

	c := discovery.Container{
		ID: i.ID, Name: t.Name, Source: SourceName, Replica: replicaKey(i.Config.Labels), Running: i.State.Running, ServerURL: t.ServerURL,
		Group: t.Group, Order: t.Order, KeepServerPath: t.KeepServerPath,
	}
	if i.State.Health != nil && i.State.Health.Status != "none" {
		c.Health = string(i.State.Health.Status)
	}
	if c.ServerURL == "" && s.cfg.TraefikServerURL {
		c.ServerURL, _ = TraefikServerURL(i.Config.Labels)
	}
	if c.Running { // a stopped container has no network settings; that is not a problem
		s.resolveAddress(&c, i.NetworkSettings, t)
	}
	return c, true
}

// replicaKey identifies the compose service a container belongs to (project + service), so that the
// containers of a scaled service are shown as one API. Empty for containers outside compose.
func replicaKey(labels map[string]string) string {
	if labels[composeNumberLabel] == "" || labels[composeServiceLabel] == "" {
		return ""
	}
	return labels[composeProjectLabel] + "/" + labels[composeServiceLabel]
}

// displayName is the name a container with unparsable labels would have had: the name label, else the compose
// service name, else the container name.
func (s *Source) displayName(labels map[string]string, containerName string) string {
	for _, candidate := range []string{labels[s.cfg.LabelPrefix+".name"], labels[composeServiceLabel]} {
		if candidate != "" {
			return candidate
		}
	}
	return containerName
}

func exposedTCPPorts(cfg *container.Config) []uint16 {
	var ports []uint16
	for p := range cfg.ExposedPorts {
		if p.Proto() == network.TCP {
			ports = append(ports, p.Num())
		}
	}
	slices.Sort(ports)
	return ports
}

// resolveAddress sets BaseURL/SpecURL (or Problem) from the published port or the network IP.
func (s *Source) resolveAddress(c *discovery.Container, ns *container.NetworkSettings, t discovery.Target) {
	if s.cfg.PreferPublishedPorts {
		if base, ok := publishedBase(ns.Ports, t.Port, t.Scheme); ok {
			c.BaseURL, c.SpecURL = base, base+t.Path
			return
		}
	}
	ip, err := s.containerIP(ns.Networks)
	if err != nil {
		c.Problem = err.Error()
		return
	}
	c.BaseURL = t.Scheme + "://" + net.JoinHostPort(ip, strconv.Itoa(int(t.Port)))
	c.SpecURL = c.BaseURL + t.Path
}

func (s *Source) containerIP(nets map[string]*network.EndpointSettings) (string, error) {
	if s.cfg.Network != "" {
		ep := nets[s.cfg.Network]
		if ep == nil || !ep.IPAddress.IsValid() {
			return "", fmt.Errorf("container is not attached to network %q", s.cfg.Network)
		}
		return ep.IPAddress.String(), nil
	}
	names := make([]string, 0, len(nets))
	for n := range nets {
		names = append(names, n)
	}
	slices.Sort(names)
	for _, n := range names {
		if ep := nets[n]; ep != nil && ep.IPAddress.IsValid() {
			return ep.IPAddress.String(), nil
		}
	}
	return "", errors.New("container has no network with an IP address")
}

// publishedBase returns <scheme>://127.0.0.1:<hostport> for a published tcp port.
func publishedBase(ports network.PortMap, port uint16, scheme string) (string, bool) {
	for p, bindings := range ports {
		if p.Num() != port || p.Proto() != network.TCP {
			continue
		}
		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}
			host := "127.0.0.1" // 0.0.0.0 / :: / unset all mean "this machine"
			if b.HostIP.IsValid() && !b.HostIP.IsUnspecified() {
				host = b.HostIP.String()
			}
			return scheme + "://" + net.JoinHostPort(host, b.HostPort), true
		}
	}
	return "", false
}
