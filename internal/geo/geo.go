// Package geo extracts the client IP and enriches it with MaxMind city/ASN
// data. Mirrors the analytics service implementation so abuse triage can
// correlate records across services.
package geo

import (
	"context"
	"encoding/json"
	"net"
	"strings"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/oschwald/geoip2-golang"
	"github.com/redis/go-redis/v9"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// Entry is the cacheable result of a geo lookup.
type Entry struct {
	CountryCode string   `json:"country_code,omitempty"`
	Region      string   `json:"region,omitempty"`
	City        string   `json:"city,omitempty"`
	Lat         *float64 `json:"lat,omitempty"`
	Lon         *float64 `json:"lon,omitempty"`
	Timezone    string   `json:"timezone,omitempty"`
	ASNNumber   uint     `json:"asn_number,omitempty"`
	ASNOrg      string   `json:"asn_org,omitempty"`
}

// Enricher resolves IPs to geo/ASN data with a Redis cache in front of
// MaxMind readers.
type Enricher struct {
	City   *geoip2.Reader
	ASN    *geoip2.Reader
	Redis  *redis.Client
	TTL    time.Duration
	Prefix string
}

// Open builds an Enricher from config.
func Open(cfg *config.Config, rdb *redis.Client) (*Enricher, error) {
	var city, asn *geoip2.Reader
	var err error
	if path := strings.TrimSpace(cfg.MaxMindCityPath); path != "" {
		city, err = geoip2.Open(path)
		if err != nil {
			return nil, err
		}
	}
	if path := strings.TrimSpace(cfg.MaxMindASNPath); path != "" {
		asn, err = geoip2.Open(path)
		if err != nil {
			if city != nil {
				_ = city.Close()
			}
			return nil, err
		}
	}
	ttl := cfg.GeoCacheTTL
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	prefix := cfg.GeoCacheKeyPrefix
	if prefix == "" {
		prefix = "media-manipulator:api:geoip:v1:"
	}
	return &Enricher{City: city, ASN: asn, Redis: rdb, TTL: ttl, Prefix: prefix}, nil
}

// Close releases MaxMind readers.
func (e *Enricher) Close() {
	if e == nil {
		return
	}
	if e.City != nil {
		_ = e.City.Close()
	}
	if e.ASN != nil {
		_ = e.ASN.Close()
	}
}

// ExtractIP returns the client IP. Prefers CF-Connecting-IP, falls back to
// X-Forwarded-For (first entry), then c.ClientIP(). Loopback addresses
// resolve to empty so MaxMind isn't called for localhost.
func ExtractIP(c *gin.Context) string {
	if value := strings.TrimSpace(c.GetHeader("CF-Connecting-IP")); value != "" {
		if ip := net.ParseIP(value); ip != nil {
			return value
		}
	}
	if value := strings.TrimSpace(c.GetHeader("X-Forwarded-For")); value != "" {
		candidate := strings.TrimSpace(strings.Split(value, ",")[0])
		if ip := net.ParseIP(candidate); ip != nil {
			return candidate
		}
	}
	ip := strings.TrimSpace(c.ClientIP())
	if ip == "::1" || ip == "127.0.0.1" {
		return ""
	}
	return ip
}

// IsLikelyPublic reports whether the parsed IP is something we should send
// to MaxMind (excludes loopback/private/link-local).
func IsLikelyPublic(ip net.IP) bool {
	return ip != nil && !ip.IsLoopback() && !ip.IsPrivate() && !ip.IsLinkLocalUnicast() && !ip.IsLinkLocalMulticast()
}

// Lookup resolves an IP string to an Entry, with Redis caching when available.
func (e *Enricher) Lookup(ctx context.Context, ipStr string) (*Entry, error) {
	if e == nil {
		return nil, nil
	}
	ipStr = strings.TrimSpace(ipStr)
	ip := net.ParseIP(ipStr)
	if !IsLikelyPublic(ip) {
		return nil, nil
	}
	cacheKey := e.Prefix + ipStr
	if e.Redis != nil {
		if raw, err := e.Redis.Get(ctx, cacheKey).Result(); err == nil && raw != "" {
			var entry Entry
			if json.Unmarshal([]byte(raw), &entry) == nil {
				return &entry, nil
			}
		}
	}
	var out Entry
	if e.City != nil {
		if rec, err := e.City.City(ip); err == nil && rec != nil {
			out.CountryCode = rec.Country.IsoCode
			if len(rec.Subdivisions) > 0 {
				if name := rec.Subdivisions[0].Names["en"]; name != "" {
					out.Region = name
				} else {
					out.Region = rec.Subdivisions[0].IsoCode
				}
			}
			out.City = rec.City.Names["en"]
			lat := rec.Location.Latitude
			lon := rec.Location.Longitude
			out.Lat = &lat
			out.Lon = &lon
			out.Timezone = rec.Location.TimeZone
		}
	}
	if e.ASN != nil {
		if rec, err := e.ASN.ASN(ip); err == nil && rec != nil {
			out.ASNNumber = rec.AutonomousSystemNumber
			out.ASNOrg = rec.AutonomousSystemOrganization
		}
	}
	if out.CountryCode == "" && out.Region == "" && out.City == "" && out.Lat == nil && out.Lon == nil && out.ASNNumber == 0 {
		return nil, nil
	}
	if e.Redis != nil {
		if body, err := json.Marshal(out); err == nil {
			_ = e.Redis.Set(ctx, cacheKey, string(body), e.TTL).Err()
		}
	}
	return &out, nil
}
