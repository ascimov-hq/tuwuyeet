package main

import (
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

type Duration struct{ time.Duration }

func (d *Duration) UnmarshalYAML(node *yaml.Node) error {
	v, err := time.ParseDuration(node.Value)
	if err != nil {
		return fmt.Errorf("invalid duration %q: %w", node.Value, err)
	}
	d.Duration = v
	return nil
}

type JobConfig struct {
	Enabled      bool   `yaml:"enabled"`
	FallbackTime string `yaml:"fallback_time"`
	Adaptive     bool   `yaml:"adaptive"`
	Force        bool   `yaml:"force"`
	OlderThan    string `yaml:"older_than"`
	IncludeLocal bool   `yaml:"include_local"`
}

type Config struct {
	Homeserver   string   `yaml:"homeserver"`
	ServerName   string   `yaml:"server_name"`
	Registration string   `yaml:"registration"`
	Timezone     string   `yaml:"timezone"`
	Listen       string   `yaml:"listen"`
	DataDir      string   `yaml:"data_dir"`
	ReplyTimeout Duration `yaml:"reply_timeout"`
	Jobs         struct {
		PruneRooms  JobConfig `yaml:"prune_rooms"`
		ExpireMedia JobConfig `yaml:"expire_media"`
	} `yaml:"jobs"`

	loc *time.Location
}

type Registration struct {
	ASToken         string `yaml:"as_token"`
	SenderLocalpart string `yaml:"sender_localpart"`
}

var mediaAge = regexp.MustCompile(`^[0-9]+[a-z]+$`)

func defaultConfig() Config {
	var c Config
	c.Homeserver = "http://homeserver:8008"
	c.Registration = "/etc/tuwuyeet/registration.yaml"
	c.Timezone = "UTC"
	c.Listen = "0.0.0.0:8095"
	c.DataDir = "/var/lib/tuwuyeet"
	c.ReplyTimeout = Duration{30 * time.Minute}
	c.Jobs.PruneRooms = JobConfig{Enabled: true, FallbackTime: "04:00", Adaptive: true}
	c.Jobs.ExpireMedia = JobConfig{Enabled: true, FallbackTime: "Sun 04:30", Adaptive: true, OlderThan: "30d"}
	return c
}

func loadConfig(path string) (Config, error) {
	c := defaultConfig()
	raw, err := os.ReadFile(path)
	if err != nil {
		return c, err
	}
	if err := yaml.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("parse %s: %w", path, err)
	}
	c.Homeserver = strings.TrimRight(c.Homeserver, "/")
	if c.ServerName == "" {
		return c, fmt.Errorf("server_name is required")
	}
	if c.loc, err = time.LoadLocation(c.Timezone); err != nil {
		return c, fmt.Errorf("timezone: %w", err)
	}
	if _, _, err := parseDailyTime(c.Jobs.PruneRooms.FallbackTime); err != nil {
		return c, fmt.Errorf("jobs.prune_rooms.fallback_time: %w", err)
	}
	if _, _, _, err := parseWeeklyTime(c.Jobs.ExpireMedia.FallbackTime); err != nil {
		return c, fmt.Errorf("jobs.expire_media.fallback_time: %w", err)
	}
	if !mediaAge.MatchString(c.Jobs.ExpireMedia.OlderThan) {
		return c, fmt.Errorf("jobs.expire_media.older_than: %q is not like 30d", c.Jobs.ExpireMedia.OlderThan)
	}
	return c, nil
}

func loadRegistration(path string) (Registration, error) {
	var r Registration
	raw, err := os.ReadFile(path)
	if err != nil {
		return r, err
	}
	if err := yaml.Unmarshal(raw, &r); err != nil {
		return r, fmt.Errorf("parse %s: %w", path, err)
	}
	if r.ASToken == "" || r.SenderLocalpart == "" {
		return r, fmt.Errorf("%s: as_token and sender_localpart are required", path)
	}
	return r, nil
}

func parseDailyTime(s string) (hour, minute int, err error) {
	h, m, ok := strings.Cut(strings.TrimSpace(s), ":")
	if !ok {
		return 0, 0, fmt.Errorf("%q is not HH:MM", s)
	}
	if hour, err = strconv.Atoi(h); err != nil || hour < 0 || hour > 23 {
		return 0, 0, fmt.Errorf("%q has a bad hour", s)
	}
	if minute, err = strconv.Atoi(m); err != nil || minute < 0 || minute > 59 {
		return 0, 0, fmt.Errorf("%q has a bad minute", s)
	}
	return hour, minute, nil
}

var weekdays = map[string]time.Weekday{
	"sun": time.Sunday, "mon": time.Monday, "tue": time.Tuesday, "wed": time.Wednesday,
	"thu": time.Thursday, "fri": time.Friday, "sat": time.Saturday,
}

func parseWeeklyTime(s string) (day time.Weekday, hour, minute int, err error) {
	d, t, ok := strings.Cut(strings.TrimSpace(s), " ")
	if !ok {
		return 0, 0, 0, fmt.Errorf("%q is not like \"Sun 04:30\"", s)
	}
	day, ok = weekdays[strings.ToLower(d)[:min(3, len(d))]]
	if !ok {
		return 0, 0, 0, fmt.Errorf("%q has a bad weekday", s)
	}
	hour, minute, err = parseDailyTime(t)
	return day, hour, minute, err
}
