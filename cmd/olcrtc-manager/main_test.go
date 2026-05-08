package main

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestSubscriptionUsesDocumentedPayloadKeys(t *testing.T) {
	cfg := Config{
		Name: "ScumVPN",
		Port: 8888,
		Locations: []Location{
			{
				Name:     "Netherlands",
				ClientID: "user",
				Endpoint: Endpoint{RoomID: "room-01", Key: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
				Carrier:  "wbstream",
				Transport: Transport{
					Type: "vp8channel",
					Payload: map[string]string{
						"vp8-fps":   "60",
						"vp8-batch": "64",
					},
				},
				Link: "direct",
				Data: "data",
				DNS:  "1.1.1.1:53",
			},
		},
	}

	got := subscription(cfg, time.Unix(1778011200, 0))

	want := "olcrtc://wbstream?vp8channel<vp8-batch=64&vp8-fps=60>@room-01#aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa%user$Netherlands"
	if !strings.Contains(got, want) {
		t.Fatalf("subscription missing URI\nwant: %s\ngot:\n%s", want, got)
	}
	if !strings.Contains(got, "#name: ScumVPN\n#update: 1778011200") {
		t.Fatalf("subscription missing global metadata:\n%s", got)
	}
	if !strings.Contains(got, "##name: Netherlands") {
		t.Fatalf("subscription missing location metadata:\n%s", got)
	}
}

func TestServerArgsMapPayloadToCLIFlags(t *testing.T) {
	loc := Location{
		ClientID: "user",
		Endpoint: Endpoint{RoomID: "room-01", Key: "key"},
		Carrier:  "jazz",
		Transport: Transport{
			Type: "seichannel",
			Payload: map[string]string{
				"fps":    "60",
				"batch":  "64",
				"frag":   "900",
				"ack-ms": "2000",
			},
		},
		Link: "direct",
		Data: "data",
		DNS:  "1.1.1.1:53",
	}

	got := strings.Join(serverArgs(loc), " ")
	for _, part := range []string{
		"-mode srv",
		"-carrier jazz",
		"-transport seichannel",
		"-id room-01",
		"-client-id user",
		"-ack-ms 2000",
		"-batch 64",
		"-fps 60",
		"-frag 900",
	} {
		if !strings.Contains(got, part) {
			t.Fatalf("server args missing %q in %q", part, got)
		}
	}
}

func TestSubscriptionForClientIncludesOnlyClientLocations(t *testing.T) {
	userLoc := testLocation("room-01", "Netherlands")
	otherLoc := testLocation("room-02", "Germany")
	otherLoc.ClientID = "other"
	cfg := testConfig(userLoc, otherLoc)

	got, ok := subscriptionForClient(cfg, "user", time.Unix(1778011200, 0))
	if !ok {
		t.Fatal("subscriptionForClient returned ok=false")
	}
	if !strings.Contains(got, "$Netherlands") {
		t.Fatalf("subscription missing user location:\n%s", got)
	}
	if strings.Contains(got, "$Germany") {
		t.Fatalf("subscription included another client's location:\n%s", got)
	}
}

func TestSubscriptionForClientRejectsUnknownClient(t *testing.T) {
	cfg := testConfig(testLocation("room-01", "Netherlands"))

	if got, ok := subscriptionForClient(cfg, "missing", time.Unix(1778011200, 0)); ok || got != "" {
		t.Fatalf("subscriptionForClient = %q, %v; want empty, false", got, ok)
	}
}

func TestSubscriptionHandlerServesClientPath(t *testing.T) {
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (process, error) {
		return process{location: loc}, nil
	})
	loc := testLocation("room-01", "Netherlands")
	if err := supervisor.StartAll(context.Background(), testConfig(loc)); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/user", nil)
	rec := httptest.NewRecorder()
	subscriptionHandler(supervisor).ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rec.Code, http.StatusOK)
	}
	if got := rec.Body.String(); !strings.Contains(got, "%user$Netherlands") {
		t.Fatalf("response missing user subscription:\n%s", got)
	}
}

func TestSubscriptionHandlerRejectsRootAndUnknownClient(t *testing.T) {
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (process, error) {
		return process{location: loc}, nil
	})
	if err := supervisor.StartAll(context.Background(), testConfig(testLocation("room-01", "Netherlands"))); err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/missing", "/user/extra"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()
		subscriptionHandler(supervisor).ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("%s status = %d, want %d", path, rec.Code, http.StatusNotFound)
		}
	}
}

func TestConfigRejectsAnyRoomID(t *testing.T) {
	cfg := Config{
		Name: "ScumVPN",
		Port: 8888,
		Locations: []Location{
			{
				ClientID:  "user",
				Endpoint:  Endpoint{RoomID: "any", Key: "key"},
				Carrier:   "wbstream",
				Transport: Transport{Type: "datachannel"},
				Link:      "direct",
				Data:      "data",
				DNS:       "1.1.1.1:53",
			},
		},
	}

	if err := cfg.Validate(); err == nil {
		t.Fatal("expected validation error for room_id=any")
	}
}

func TestTransportUnmarshalPayload(t *testing.T) {
	var cfg Config
	data := []byte(`{
		"version": 4,
		"name": "ScumVPN",
		"port": 8888,
		"locations": [{
			"name": "Netherlands",
			"client-id": "user",
			"endpoint": {"room_id": "room-01", "key": "key"},
			"carrier": "wbstream",
			"transport": {
				"type": "vp8channel",
				"payload": {
					"vp8-fps": 60,
					"vp8-batch": 64
				}
			},
			"link": "direct",
			"data": "data",
			"dns": "1.1.1.1:53"
		}]
	}`)

	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if got := cfg.Locations[0].Transport.Payload["vp8-fps"]; got != "60" {
		t.Fatalf("vp8-fps = %q, want 60", got)
	}
}

func TestConfigUnmarshalClientsFormat(t *testing.T) {
	var cfg Config
	data := []byte(`{
		"vesion": 1,
		"name": "ScumVPN",
		"port": 8888,
		"clients": [{
			"client-id": "mark",
			"locations": [
				{
					"name": "Netherlands",
					"carrier": "wbstream",
					"transport": {"type": "datachannel"},
					"link": "direct",
					"data": "data",
					"dns": "1.1.1.1:53",
					"endpoint": {"room_id": "room-01", "key": "key"}
				},
				{
					"name": "Netherlands VP8",
					"carrier": "wbstream",
					"transport": {
						"type": "vp8channel",
						"payload": {
							"vp8-fps": 60,
							"vp8-batch": 64
						}
					},
					"link": "direct",
					"data": "data",
					"dns": "1.1.1.1:53",
					"endpoint": {"room_id": "room-02", "key": "key"}
				}
			]
		}]
	}`)

	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	if cfg.Version != 1 {
		t.Fatalf("Version = %d, want 1", cfg.Version)
	}
	if len(cfg.Locations) != 2 {
		t.Fatalf("locations = %d, want 2", len(cfg.Locations))
	}
	if got := cfg.Locations[0].ClientID; got != "mark" {
		t.Fatalf("client-id = %q, want mark", got)
	}
	if got := cfg.Locations[1].Transport.Payload["vp8-fps"]; got != "60" {
		t.Fatalf("vp8-fps = %q, want 60", got)
	}
}

func TestSupervisorReloadStartsAddedLocationAndUpdatesSubscription(t *testing.T) {
	loc1 := testLocation("room-01", "Netherlands")
	loc2 := testLocation("room-02", "Germany")
	started := make([]string, 0)
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (process, error) {
		started = append(started, locationKey(loc))
		return process{location: loc}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc1)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(loc1, loc2)); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(started, ","); got != "user:room-01:datachannel,user:room-02:datachannel" {
		t.Fatalf("started = %q, want user:room-01:datachannel,user:room-02:datachannel", got)
	}
	if got := supervisor.Subscription(time.Unix(1778011200, 0)); !strings.Contains(got, "$Germany") {
		t.Fatalf("subscription was not updated:\n%s", got)
	}
}

func TestSupervisorReloadRestartsChangedLocation(t *testing.T) {
	loc := testLocation("room-01", "Netherlands")
	changed := loc
	changed.Endpoint.RoomID = "room-02"
	started := make([]string, 0)
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (process, error) {
		started = append(started, loc.Endpoint.RoomID)
		return process{location: loc}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(changed)); err != nil {
		t.Fatal(err)
	}

	if got := strings.Join(started, ","); got != "room-01,room-02" {
		t.Fatalf("started room ids = %q, want room-01,room-02", got)
	}
	if got := supervisor.Subscription(time.Unix(1778011200, 0)); !strings.Contains(got, "@room-02#") {
		t.Fatalf("subscription did not use changed location:\n%s", got)
	}
}

func TestSupervisorReloadFailureKeepsCurrentConfig(t *testing.T) {
	loc1 := testLocation("room-01", "Netherlands")
	loc2 := testLocation("room-02", "Germany")
	startErr := errors.New("boom")
	supervisor := NewSupervisor("olcrtc", func(ctx context.Context, path string, loc Location) (process, error) {
		if loc.Endpoint.RoomID == "room-02" {
			return process{}, startErr
		}
		return process{location: loc}, nil
	})

	if err := supervisor.StartAll(context.Background(), testConfig(loc1)); err != nil {
		t.Fatal(err)
	}
	if err := supervisor.Reload(context.Background(), testConfig(loc1, loc2)); !errors.Is(err, startErr) {
		t.Fatalf("Reload error = %v, want %v", err, startErr)
	}

	if got := supervisor.Subscription(time.Unix(1778011200, 0)); strings.Contains(got, "$Germany") {
		t.Fatalf("failed reload changed subscription:\n%s", got)
	}
}

func testConfig(locations ...Location) Config {
	return Config{
		Name:      "ScumVPN",
		Port:      8888,
		Locations: locations,
	}
}

func testLocation(roomID, name string) Location {
	return Location{
		Name:      name,
		ClientID:  "user",
		Endpoint:  Endpoint{RoomID: roomID, Key: "key"},
		Carrier:   "wbstream",
		Transport: Transport{Type: "datachannel"},
		Link:      "direct",
		Data:      "data",
		DNS:       "1.1.1.1:53",
	}
}
