package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Config struct {
	Version          int        `json:"version"`
	LegacyVersion    int        `json:"vesion"`
	Name             string     `json:"name"`
	Port             int        `json:"port"`
	ActiveLocationID string     `json:"active_location_id"`
	Clients          []Client   `json:"clients"`
	Locations        []Location `json:"locations"`
}

func (c *Config) UnmarshalJSON(data []byte) error {
	type config Config
	var parsed config
	if err := json.Unmarshal(data, &parsed); err != nil {
		return err
	}
	*c = Config(parsed)
	c.Normalize()
	return nil
}

type Client struct {
	ClientID  string     `json:"client-id"`
	Locations []Location `json:"locations"`
}

type Location struct {
	Name      string    `json:"name"`
	ClientID  string    `json:"client-id"`
	Endpoint  Endpoint  `json:"endpoint"`
	Carrier   string    `json:"carrier"`
	Transport Transport `json:"transport"`
	Link      string    `json:"link"`
	Data      string    `json:"data"`
	DNS       string    `json:"dns"`
}

type Endpoint struct {
	RoomID string `json:"room_id"`
	Key    string `json:"key"`
}

type Transport struct {
	Type    string
	Payload map[string]string
}

func (t *Transport) UnmarshalJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	var typ string
	if err := json.Unmarshal(raw["type"], &typ); err != nil {
		return fmt.Errorf("transport.type: %w", err)
	}

	payload := make(map[string]string)
	for key, value := range raw {
		if key == "type" {
			continue
		}

		if key == "payload" {
			var nested map[string]any
			if err := json.Unmarshal(value, &nested); err != nil {
				return fmt.Errorf("transport.payload: %w", err)
			}
			for payloadKey, payloadValue := range nested {
				payload[payloadKey] = fmt.Sprint(payloadValue)
			}
			continue
		}

		var scalar any
		if err := json.Unmarshal(value, &scalar); err != nil {
			return fmt.Errorf("transport.%s: %w", key, err)
		}
		payload[key] = fmt.Sprint(scalar)
	}

	t.Type = typ
	t.Payload = payload
	return nil
}

type process struct {
	location Location
	cmd      *exec.Cmd
}

type starter func(context.Context, string, Location) (process, error)

type Supervisor struct {
	mu         sync.RWMutex
	cfg        Config
	olcrtcPath string
	processes  map[string]process
	start      starter
}

func main() {
	if err := run(); err != nil {
		log.Fatal(err)
	}
}

func run() error {
	var configPath string
	var port int
	flag.StringVar(&configPath, "config", "", "path to olcrtc-manager JSON config")
	flag.IntVar(&port, "port", 0, "HTTP listen port; overrides config.port")
	flag.Parse()

	if configPath == "" {
		return errors.New("-config is required")
	}

	cfg, err := loadConfig(configPath)
	if err != nil {
		return err
	}
	if port != 0 {
		cfg.Port = port
	}
	if err := cfg.Validate(); err != nil {
		return err
	}

	olcrtcPath, err := resolveOlcrtcPath()
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	supervisor := NewSupervisor(olcrtcPath, startInstance)
	if err := supervisor.StartAll(ctx, cfg); err != nil {
		return err
	}
	defer supervisor.StopAll()

	reloadc := make(chan os.Signal, 1)
	signal.Notify(reloadc, syscall.SIGHUP)
	defer signal.Stop(reloadc)

	reload := func() error {
		reloaded, err := loadConfig(configPath)
		if err != nil {
			return err
		}
		if port != 0 {
			reloaded.Port = port
		}
		if reloaded.Port != cfg.Port {
			return fmt.Errorf("reload cannot change port from %d to %d", cfg.Port, reloaded.Port)
		}
		if err := reloaded.Validate(); err != nil {
			return err
		}
		return supervisor.Reload(ctx, reloaded)
	}

	handler := http.NewServeMux()
	handler.HandleFunc("/-/reload", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", http.MethodPost)
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !isLoopbackRequest(r) {
			http.Error(w, "reload is only allowed from loopback", http.StatusForbidden)
			return
		}
		if err := reload(); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	})
	handler.Handle("/", subscriptionHandler(supervisor))

	server := &http.Server{
		Addr:              ":" + strconv.Itoa(cfg.Port),
		Handler:           handler,
		ReadHeaderTimeout: 5 * time.Second,
	}

	errc := make(chan error, 1)
	go func() {
		log.Printf("serving subscription on :%d", cfg.Port)
		if err := server.ListenAndServe(); !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	for {
		select {
		case <-ctx.Done():
			shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			return server.Shutdown(shutdownCtx)
		case <-reloadc:
			if err := reload(); err != nil {
				log.Printf("reload failed: %v", err)
				continue
			}
			log.Printf("reload completed")
		case err := <-errc:
			return err
		}
	}
}

func NewSupervisor(olcrtcPath string, start starter) *Supervisor {
	return &Supervisor{
		olcrtcPath: olcrtcPath,
		processes:  make(map[string]process),
		start:      start,
	}
}

func (s *Supervisor) StartAll(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	for _, loc := range cfg.Locations {
		p, err := s.start(ctx, s.olcrtcPath, loc)
		if err != nil {
			stopProcessMap(s.processes)
			s.processes = make(map[string]process)
			return err
		}
		s.processes[locationKey(loc)] = p
	}
	s.cfg = cfg
	return nil
}

func (s *Supervisor) Reload(ctx context.Context, cfg Config) error {
	if err := cfg.Validate(); err != nil {
		return err
	}

	next := locationsByKey(cfg.Locations)

	s.mu.Lock()
	defer s.mu.Unlock()

	current := locationsByKey(s.cfg.Locations)
	started := make(map[string]process)

	for id, nextLoc := range next {
		currentLoc, exists := current[id]
		if exists && reflect.DeepEqual(currentLoc, nextLoc) {
			continue
		}

		p, err := s.start(ctx, s.olcrtcPath, nextLoc)
		if err != nil {
			stopProcessMap(started)
			return err
		}
		started[id] = p
	}

	for id, currentLoc := range current {
		nextLoc, exists := next[id]
		if !exists || !reflect.DeepEqual(currentLoc, nextLoc) {
			s.stopLocked(id)
		}
	}

	for id, p := range started {
		s.processes[id] = p
	}
	s.cfg = cfg
	return nil
}

func (s *Supervisor) StopAll() {
	s.mu.Lock()
	defer s.mu.Unlock()
	stopProcessMap(s.processes)
	s.processes = make(map[string]process)
}

func (s *Supervisor) Subscription(now time.Time) string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return subscription(s.cfg, now)
}

func (s *Supervisor) SubscriptionForClient(clientID string, now time.Time) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return subscriptionForClient(s.cfg, clientID, now)
}

func (s *Supervisor) stopLocked(id string) {
	p, ok := s.processes[id]
	if !ok {
		return
	}
	stopProcess(p)
	delete(s.processes, id)
}

func locationsByKey(locations []Location) map[string]Location {
	byKey := make(map[string]Location, len(locations))
	for _, loc := range locations {
		byKey[locationKey(loc)] = loc
	}
	return byKey
}

func stopProcessMap(processes map[string]process) {
	for _, p := range processes {
		stopProcess(p)
	}
}

func startInstance(ctx context.Context, olcrtcPath string, loc Location) (process, error) {
	args := serverArgs(loc)
	cmd := exec.CommandContext(ctx, olcrtcPath, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return process{}, fmt.Errorf("start olcrtc for %s: %w", locationKey(loc), err)
	}

	p := process{location: loc, cmd: cmd}
	log.Printf("started olcrtc for %s: %s %s", locationKey(loc), olcrtcPath, strings.Join(args, " "))

	go func() {
		if err := cmd.Wait(); err != nil && ctx.Err() == nil {
			log.Printf("olcrtc for %s exited: %v", locationKey(loc), err)
		}
	}()

	return p, nil
}

func stopProcess(p process) {
	if p.cmd == nil || p.cmd.Process == nil {
		return
	}
	_ = p.cmd.Process.Signal(syscall.SIGTERM)
}

func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func startInstances(ctx context.Context, olcrtcPath string, locations []Location) ([]process, error) {
	processes := make([]process, 0, len(locations))
	for _, loc := range locations {
		p, err := startInstance(ctx, olcrtcPath, loc)
		if err != nil {
			stopInstances(processes)
			return nil, err
		}
		processes = append(processes, p)
	}
	return processes, nil
}

func stopInstances(processes []process) {
	for _, p := range processes {
		stopProcess(p)
	}
}

func loadConfig(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config: %w", err)
	}

	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("parse config: %w", err)
	}
	cfg.Normalize()
	return cfg, nil
}

func (c *Config) Normalize() {
	if c.Version == 0 && c.LegacyVersion != 0 {
		c.Version = c.LegacyVersion
	}

	if len(c.Clients) == 0 {
		return
	}

	locations := make([]Location, 0)
	for _, client := range c.Clients {
		for _, loc := range client.Locations {
			if loc.ClientID == "" {
				loc.ClientID = client.ClientID
			}
			locations = append(locations, loc)
		}
	}
	c.Locations = locations
}

func (c Config) Validate() error {
	if c.Port <= 0 || c.Port > 65535 {
		return fmt.Errorf("port must be between 1 and 65535, got %d", c.Port)
	}
	if len(c.Locations) == 0 {
		return errors.New("locations must not be empty")
	}

	ids := make(map[string]struct{}, len(c.Locations))
	for i, loc := range c.Locations {
		prefix := fmt.Sprintf("locations[%d]", i)
		if loc.ClientID == "" {
			return fmt.Errorf("%s.client-id is required", prefix)
		}
		if loc.Endpoint.RoomID == "" || loc.Endpoint.RoomID == "any" {
			return fmt.Errorf("%s.endpoint.room_id must be concrete", prefix)
		}
		if loc.Endpoint.Key == "" {
			return fmt.Errorf("%s.endpoint.key is required", prefix)
		}
		if loc.Carrier == "" {
			return fmt.Errorf("%s.carrier is required", prefix)
		}
		if loc.Transport.Type == "" {
			return fmt.Errorf("%s.transport.type is required", prefix)
		}
		key := locationKey(loc)
		if _, exists := ids[key]; exists {
			return fmt.Errorf("%s location key %q is duplicated", prefix, key)
		}
		ids[key] = struct{}{}
		if !isSupported(loc.Carrier, loc.Transport.Type) {
			return fmt.Errorf("%s: unsupported carrier/transport combination %s + %s", prefix, loc.Carrier, loc.Transport.Type)
		}
		if err := validatePayload(loc.Transport); err != nil {
			return fmt.Errorf("%s.transport: %w", prefix, err)
		}
		if loc.Link == "" {
			return fmt.Errorf("%s.link is required", prefix)
		}
		if loc.Data == "" {
			return fmt.Errorf("%s.data is required", prefix)
		}
		if loc.DNS == "" {
			return fmt.Errorf("%s.dns is required", prefix)
		}
	}
	return nil
}

func locationKey(loc Location) string {
	return strings.Join([]string{loc.ClientID, loc.Endpoint.RoomID, loc.Transport.Type}, ":")
}

func isSupported(carrier, transport string) bool {
	matrix := map[string]map[string]bool{
		"telemost": {
			"datachannel":  false,
			"vp8channel":   true,
			"seichannel":   false,
			"videochannel": true,
		},
		"jazz": {
			"datachannel":  true,
			"vp8channel":   true,
			"seichannel":   true,
			"videochannel": true,
		},
		"wbstream": {
			"datachannel":  true,
			"vp8channel":   true,
			"seichannel":   true,
			"videochannel": true,
		},
	}
	return matrix[carrier][transport]
}

func validatePayload(t Transport) error {
	allowed := map[string]map[string]struct{}{
		"datachannel":  {},
		"vp8channel":   {"vp8-fps": {}, "vp8-batch": {}},
		"seichannel":   {"fps": {}, "batch": {}, "frag": {}, "ack-ms": {}},
		"videochannel": {"video-w": {}, "video-h": {}, "video-fps": {}, "video-bitrate": {}, "video-hw": {}, "video-codec": {}, "video-qr-size": {}, "video-qr-recovery": {}, "video-tile-module": {}, "video-tile-rs": {}},
	}

	keys, ok := allowed[t.Type]
	if !ok {
		return fmt.Errorf("unknown transport %q", t.Type)
	}
	for key := range t.Payload {
		if _, ok := keys[key]; !ok {
			return fmt.Errorf("unsupported payload key %q for %s", key, t.Type)
		}
	}
	return nil
}

func resolveOlcrtcPath() (string, error) {
	if path := os.Getenv("OLCRTC_PATH"); path != "" {
		return path, nil
	}

	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("resolve executable path: %w", err)
	}
	return filepath.Join(filepath.Dir(exe), "olcrtc"), nil
}

func serverArgs(loc Location) []string {
	args := []string{
		"-mode", "srv",
		"-carrier", loc.Carrier,
		"-transport", loc.Transport.Type,
		"-id", loc.Endpoint.RoomID,
		"-client-id", loc.ClientID,
		"-key", loc.Endpoint.Key,
		"-link", loc.Link,
		"-data", loc.Data,
		"-dns", loc.DNS,
	}

	for _, key := range sortedKeys(loc.Transport.Payload) {
		args = append(args, "-"+key, loc.Transport.Payload[key])
	}
	return args
}

func subscriptionHandler(supervisor *Supervisor) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		clientID, ok := clientIDFromPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}

		sub, ok := supervisor.SubscriptionForClient(clientID, time.Now())
		if !ok {
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = w.Write([]byte(sub))
	})
}

func clientIDFromPath(path string) (string, bool) {
	clientID := strings.TrimPrefix(path, "/")
	if clientID == "" || strings.Contains(clientID, "/") {
		return "", false
	}
	return clientID, true
}

func subscription(cfg Config, now time.Time) string {
	return subscriptionForLocations(cfg.Name, cfg.Locations, now)
}

func subscriptionForClient(cfg Config, clientID string, now time.Time) (string, bool) {
	locations := make([]Location, 0)
	for _, loc := range cfg.Locations {
		if loc.ClientID == clientID {
			locations = append(locations, loc)
		}
	}
	if len(locations) == 0 {
		return "", false
	}
	return subscriptionForLocations(cfg.Name, locations, now), true
}

func subscriptionForLocations(name string, locations []Location, now time.Time) string {
	var b bytes.Buffer
	if name != "" {
		fmt.Fprintf(&b, "#name: %s\n", name)
	}
	fmt.Fprintf(&b, "#update: %d\n\n", now.Unix())

	for _, loc := range locations {
		fmt.Fprintln(&b, locationURI(loc))
		if loc.Name != "" {
			fmt.Fprintf(&b, "##name: %s\n", loc.Name)
		}
		fmt.Fprintln(&b)
	}
	return b.String()
}

func locationURI(loc Location) string {
	payload := payloadString(loc.Transport.Payload)
	return fmt.Sprintf("olcrtc://%s?%s%s@%s#%s%%%s$%s",
		loc.Carrier,
		loc.Transport.Type,
		payload,
		loc.Endpoint.RoomID,
		loc.Endpoint.Key,
		loc.ClientID,
		loc.Name,
	)
}

func payloadString(payload map[string]string) string {
	if len(payload) == 0 {
		return ""
	}

	parts := make([]string, 0, len(payload))
	for _, key := range sortedKeys(payload) {
		parts = append(parts, key+"="+payload[key])
	}
	return "<" + strings.Join(parts, "&") + ">"
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}
