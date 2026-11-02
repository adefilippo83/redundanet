// Package manifest reads, checks and signs the network manifest of RFC 0001:
// a YAML file in a Git repository, reviewed by people, that lists the
// operators, the hubs, the members and the storage devices, and takes effect
// on a node only when an operator's signature over it verifies.
//
// During the migration the same file also carries the RedundaNet 2 keys
// (`nodes`, `introducer_furl`, the VPN and Tahoe settings under `network`).
// This package ignores exactly those keys and refuses any other it does not
// know, so a typo in a v3 key is caught and a v2 key is never mistaken for
// one.
package manifest

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	"github.com/adefilippo83/redundanet/agent/internal/identity"
	"github.com/adefilippo83/redundanet/agent/internal/semver"
)

// Schema is the manifest schema this package reads.
const Schema = 3

// Member statuses.
const (
	StatusActive  = "active"
	StatusLeaving = "leaving"
)

// Manifest is the parsed v3 content.
type Manifest struct {
	Schema    int
	Revision  uint64
	Network   Network
	Operators []Operator
	Hubs      []Hub
	Members   []Member
	Storage   []StorageDevice
}

// Network is the `network` section.
type Network struct {
	Name                string
	Encoding            Encoding
	EncodingTransition  *Transition
	ReservePercent      int
	GraceDays           int
	MinVersion          string
	SignatureMaxAgeDays int
	QuotaHeadroom       int
	ObjectsPerGB        int
	RepairAfterHours    int
}

// Encoding is k-of-n.
type Encoding struct {
	K int `yaml:"k"`
	N int `yaml:"n"`
}

// Transition is a k-of-n change in progress.
type Transition struct {
	From     Encoding `yaml:"from"`
	To       Encoding `yaml:"to"`
	Deadline string   `yaml:"deadline"`
}

// Operator is a key whose signature makes a manifest take effect.
type Operator struct {
	Name string
	Key  identity.Key
}

// Hub is a relay, directory, collector and join endpoint.
type Hub struct {
	Name   string
	Relay  string // host:port of the QUIC relay
	TCP    string // host:port of the TLS carrier; empty means port 443 on the relay's host
	Status string // URL of the status page
	Join   string // URL of the join endpoint
	Key    identity.Key
}

// TCPAddr is where the hub's TLS carrier listens: the `tcp` entry, or the
// RFC's rule, port 443 on the relay's host.
func (h Hub) TCPAddr() string {
	if h.TCP != "" {
		return h.TCP
	}
	host := h.Relay
	if i := strings.LastIndex(h.Relay, ":"); i > 0 {
		host = h.Relay[:i]
	}
	return host + ":443"
}

// Member is one entry of the `members` list.
type Member struct {
	Name           string
	Key            identity.Key
	ContributionGB int
	AllocationGB   *int // client-only members: the allocation the community granted
	Sponsor        *Sponsor
	VouchedBy      []Vouch
	LegacyPGP      *LegacyPGP
	Joined         string
	Status         string
	LeavingSince   string
}

// Sponsor names whose contribution, or the reserve, pays for a client-only
// member's allocation.
type Sponsor struct {
	Member string `yaml:"member"` // a member name, or "reserve"
	GB     int    `yaml:"gb"`
}

// Vouch is an invite and its confirmation, both signed by the inviter's
// device, kept so anyone with the manifest can verify who vouched for whom.
type Vouch struct {
	Member     string `yaml:"member"`      // the inviter
	DeviceCert string `yaml:"device_cert"` // base64 of the inviter's signed device certificate
	Nonce      string `yaml:"nonce"`       // base64, from the invite
	Expiry     string `yaml:"expiry"`      // RFC 3339, from the invite
	InviteSig  string `yaml:"invite_sig"`  // base64
	ConfirmSig string `yaml:"confirm_sig"` // base64
}

// LegacyPGP is a RedundaNet 2 member's PGP signature over their member key.
type LegacyPGP struct {
	Fingerprint string `yaml:"fingerprint"`
	Signature   string `yaml:"signature"`
}

// StorageDevice is one entry of the `storage` list.
type StorageDevice struct {
	Member  string
	Device  identity.Key
	Name    string
	UDPPort int // optional: a port the member forwards on their router
}

// The raw shapes decode the YAML; the exported types above are built from
// them with the keys parsed and the v2 keys set aside.

type rawFile struct {
	Schema    *int           `yaml:"schema"`
	Revision  *uint64        `yaml:"revision"`
	Network   *rawNetwork    `yaml:"network"`
	Operators []rawOperator  `yaml:"operators"`
	Hubs      []rawHub       `yaml:"hubs"`
	Members   []rawMember    `yaml:"members"`
	Storage   []rawStorage   `yaml:"storage"`
	Rest      map[string]any `yaml:",inline"`
}

type rawNetwork struct {
	Name                string         `yaml:"name"`
	Encoding            *Encoding      `yaml:"encoding"`
	EncodingTransition  *Transition    `yaml:"encoding_transition"`
	ReservePercent      *int           `yaml:"reserve_percent"`
	GraceDays           *int           `yaml:"grace_days"`
	MinVersion          string         `yaml:"min_version"`
	SignatureMaxAgeDays *int           `yaml:"signature_max_age_days"`
	QuotaHeadroom       *int           `yaml:"quota_headroom"`
	ObjectsPerGB        *int           `yaml:"objects_per_gb"`
	RepairAfterHours    *int           `yaml:"repair_after_hours"`
	Rest                map[string]any `yaml:",inline"`
}

type rawOperator struct {
	Name string `yaml:"name"`
	Key  string `yaml:"key"`
}

type rawHub struct {
	Name   string `yaml:"name"`
	Relay  string `yaml:"relay"`
	TCP    string `yaml:"tcp"`
	Status string `yaml:"status"`
	Join   string `yaml:"join"`
	Key    string `yaml:"key"`
}

type rawMember struct {
	Name           string     `yaml:"name"`
	Key            string     `yaml:"key"`
	ContributionGB *int       `yaml:"contribution_gb"`
	AllocationGB   *int       `yaml:"allocation_gb"`
	Sponsor        *Sponsor   `yaml:"sponsor"`
	VouchedBy      []Vouch    `yaml:"vouched_by"`
	LegacyPGP      *LegacyPGP `yaml:"legacy_pgp"`
	Joined         string     `yaml:"joined"`
	Status         string     `yaml:"status"`
	LeavingSince   string     `yaml:"leaving_since"`
}

type rawStorage struct {
	Member  string `yaml:"member"`
	Device  string `yaml:"device"`
	Name    string `yaml:"name"`
	UDPPort int    `yaml:"udp_port"`
}

// v2Keys are the RedundaNet 2 keys a transitional manifest carries beside
// the v3 ones. Anything else unknown is an error.
var (
	v2TopKeys     = map[string]bool{"nodes": true, "introducer_furl": true}
	v2NetworkKeys = map[string]bool{"version": true, "domain": true, "vpn_network": true, "tahoe": true, "quota": true}
)

// Parse decodes a manifest. It reports shape errors: missing sections, keys
// that are neither v3 nor v2, malformed keys and dates. Validate reports
// everything else.
func Parse(data []byte) (*Manifest, error) {
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var raw rawFile
	if err := dec.Decode(&raw); err != nil {
		return nil, fmt.Errorf("manifest: %w", err)
	}
	if err := unknownKeys("", raw.Rest, v2TopKeys); err != nil {
		return nil, err
	}
	if raw.Schema == nil {
		return nil, errors.New("manifest: no `schema` key: this is not a version 3 manifest")
	}
	if *raw.Schema != Schema {
		return nil, fmt.Errorf("manifest: schema %d, this binary reads %d", *raw.Schema, Schema)
	}
	if raw.Revision == nil {
		return nil, errors.New("manifest: `revision` is required")
	}
	if raw.Network == nil {
		return nil, errors.New("manifest: `network` is required")
	}
	if err := unknownKeys("network.", raw.Network.Rest, v2NetworkKeys); err != nil {
		return nil, err
	}
	m := &Manifest{Schema: *raw.Schema, Revision: *raw.Revision}
	n := raw.Network
	m.Network = Network{
		Name:                n.Name,
		EncodingTransition:  n.EncodingTransition,
		MinVersion:          n.MinVersion,
		ReservePercent:      intOr(n.ReservePercent, 15),
		GraceDays:           intOr(n.GraceDays, 30),
		SignatureMaxAgeDays: intOr(n.SignatureMaxAgeDays, 90),
		QuotaHeadroom:       intOr(n.QuotaHeadroom, 2),
		ObjectsPerGB:        intOr(n.ObjectsPerGB, 1000),
		RepairAfterHours:    intOr(n.RepairAfterHours, 72),
	}
	if n.Encoding == nil {
		return nil, errors.New("manifest: `network.encoding` is required")
	}
	m.Network.Encoding = *n.Encoding
	for i, o := range raw.Operators {
		k, err := identity.ParseKey(o.Key)
		if err != nil {
			return nil, fmt.Errorf("manifest: operators[%d] %q: %w", i, o.Name, err)
		}
		m.Operators = append(m.Operators, Operator{Name: o.Name, Key: k})
	}
	for i, h := range raw.Hubs {
		k, err := identity.ParseKey(h.Key)
		if err != nil {
			return nil, fmt.Errorf("manifest: hubs[%d] %q: %w", i, h.Name, err)
		}
		m.Hubs = append(m.Hubs, Hub{Name: h.Name, Relay: h.Relay, TCP: h.TCP, Status: h.Status, Join: h.Join, Key: k})
	}
	for i, r := range raw.Members {
		k, err := identity.ParseKey(r.Key)
		if err != nil {
			return nil, fmt.Errorf("manifest: members[%d] %q: %w", i, r.Name, err)
		}
		mem := Member{Name: r.Name, Key: k, AllocationGB: r.AllocationGB, Sponsor: r.Sponsor, VouchedBy: r.VouchedBy,
			LegacyPGP: r.LegacyPGP, Joined: r.Joined, Status: r.Status, LeavingSince: r.LeavingSince}
		if r.ContributionGB != nil {
			mem.ContributionGB = *r.ContributionGB
		}
		m.Members = append(m.Members, mem)
	}
	for i, s := range raw.Storage {
		k, err := identity.ParseKey(s.Device)
		if err != nil {
			return nil, fmt.Errorf("manifest: storage[%d] %q: %w", i, s.Name, err)
		}
		m.Storage = append(m.Storage, StorageDevice{Member: s.Member, Device: k, Name: s.Name, UDPPort: s.UDPPort})
	}
	return m, nil
}

func intOr(p *int, def int) int {
	if p == nil {
		return def
	}
	return *p
}

func unknownKeys(prefix string, rest map[string]any, allowed map[string]bool) error {
	var bad []string
	for k := range rest {
		if !allowed[k] {
			bad = append(bad, prefix+k)
		}
	}
	if len(bad) == 0 {
		return nil
	}
	sort.Strings(bad)
	return fmt.Errorf("manifest: unknown keys: %s", strings.Join(bad, ", "))
}

// Problem is one finding of Validate.
type Problem struct {
	Level string // "error" or "warning"
	Msg   string
}

func (p Problem) String() string { return p.Level + ": " + p.Msg }

// Problems is the result of Validate.
type Problems []Problem

// Errors returns the blocking problems.
func (ps Problems) Errors() []Problem {
	var out []Problem
	for _, p := range ps {
		if p.Level == "error" {
			out = append(out, p)
		}
	}
	return out
}

func (ps *Problems) errorf(format string, args ...any) {
	*ps = append(*ps, Problem{"error", fmt.Sprintf(format, args...)})
}

func (ps *Problems) warnf(format string, args ...any) {
	*ps = append(*ps, Problem{"warning", fmt.Sprintf(format, args...)})
}

var (
	nameRe = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	dateRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}$`)
)

// Validate applies the rules of the RFC's Membership section. Errors make
// the manifest unusable; warnings are capacity and policy nudges.
func (m *Manifest) Validate() Problems {
	var ps Problems
	n := m.Network
	if m.Revision == 0 {
		ps.errorf("revision must be at least 1")
	}
	if !nameRe.MatchString(n.Name) {
		ps.errorf("network.name %q: lowercase letters, digits and dashes only", n.Name)
	}
	if n.Encoding.K < 1 || n.Encoding.N < n.Encoding.K {
		ps.errorf("network.encoding: k must be at least 1 and n at least k, got %d-of-%d", n.Encoding.K, n.Encoding.N)
	}
	if t := n.EncodingTransition; t != nil {
		if t.From.K < 1 || t.From.N < t.From.K || t.To.K < 1 || t.To.N < t.To.K {
			ps.errorf("network.encoding_transition: both encodings must be valid")
		}
		if t.To != n.Encoding {
			ps.errorf("network.encoding_transition.to must equal network.encoding")
		}
		if !dateRe.MatchString(t.Deadline) {
			ps.errorf("network.encoding_transition.deadline must be a date (YYYY-MM-DD)")
		}
	}
	if n.ReservePercent < 0 || n.ReservePercent > 50 {
		ps.errorf("network.reserve_percent must be between 0 and 50")
	}
	if n.GraceDays < 1 {
		ps.errorf("network.grace_days must be at least 1")
	}
	if n.MinVersion != "" && !semver.Valid(n.MinVersion) {
		ps.errorf("network.min_version %q is not MAJOR.MINOR.PATCH with an optional pre-release suffix", n.MinVersion)
	}
	if n.SignatureMaxAgeDays < 1 {
		ps.errorf("network.signature_max_age_days must be at least 1")
	}
	if n.QuotaHeadroom < 1 {
		ps.errorf("network.quota_headroom must be at least 1")
	}
	if n.ObjectsPerGB < 1 {
		ps.errorf("network.objects_per_gb must be at least 1")
	}
	if n.RepairAfterHours < 1 {
		ps.errorf("network.repair_after_hours must be at least 1")
	}

	if len(m.Operators) == 0 {
		ps.errorf("operators: at least one operator key is required")
	}
	seenKeys := map[identity.Key]string{}
	seenNames := map[string]bool{}
	for _, o := range m.Operators {
		if !nameRe.MatchString(o.Name) {
			ps.errorf("operators: name %q: lowercase letters, digits and dashes only", o.Name)
		}
		if who, dup := seenKeys[o.Key]; dup {
			ps.errorf("operators: key of %q already used by %s", o.Name, who)
		}
		seenKeys[o.Key] = "operator " + o.Name
	}
	for _, h := range m.Hubs {
		if !nameRe.MatchString(h.Name) || seenNames["hub:"+h.Name] {
			ps.errorf("hubs: name %q is invalid or duplicated", h.Name)
		}
		seenNames["hub:"+h.Name] = true
		if !strings.Contains(h.Relay, ":") {
			ps.errorf("hubs: %q: relay must be host:port", h.Name)
		}
		if h.TCP != "" && !strings.Contains(h.TCP, ":") {
			ps.errorf("hubs: %q: tcp must be host:port", h.Name)
		}
		for _, u := range []string{h.Status, h.Join} {
			if u != "" && !strings.HasPrefix(u, "https://") {
				ps.errorf("hubs: %q: status and join must be https:// URLs", h.Name)
			}
		}
		if who, dup := seenKeys[h.Key]; dup {
			ps.errorf("hubs: key of %q already used by %s", h.Name, who)
		}
		seenKeys[h.Key] = "hub " + h.Name
	}
	if len(m.Hubs) == 0 {
		ps.warnf("hubs: no hub listed; devices can only reach peers with a direct path")
	}

	members := map[string]*Member{}
	for i := range m.Members {
		mem := &m.Members[i]
		if !nameRe.MatchString(mem.Name) {
			ps.errorf("members: name %q: lowercase letters, digits and dashes only", mem.Name)
		}
		if members[mem.Name] != nil {
			ps.errorf("members: name %q is duplicated", mem.Name)
		}
		members[mem.Name] = mem
		if who, dup := seenKeys[mem.Key]; dup {
			ps.errorf("members: key of %q already used by %s", mem.Name, who)
		}
		seenKeys[mem.Key] = "member " + mem.Name
		switch mem.Status {
		case StatusActive:
			if mem.LeavingSince != "" {
				ps.errorf("members: %q is active but has leaving_since", mem.Name)
			}
		case StatusLeaving:
			if !dateRe.MatchString(mem.LeavingSince) {
				ps.errorf("members: %q is leaving without a leaving_since date", mem.Name)
			}
		default:
			ps.errorf("members: %q: status must be active or leaving, got %q", mem.Name, mem.Status)
		}
		if mem.Joined != "" && !dateRe.MatchString(mem.Joined) {
			ps.errorf("members: %q: joined must be a date (YYYY-MM-DD)", mem.Name)
		}
		if mem.ContributionGB < 0 {
			ps.errorf("members: %q: contribution_gb is negative", mem.Name)
		}
		if mem.ContributionGB == 0 {
			if mem.AllocationGB == nil || mem.Sponsor == nil {
				ps.errorf("members: %q contributes nothing and needs allocation_gb and a sponsor", mem.Name)
			} else if mem.Sponsor.GB < *mem.AllocationGB {
				ps.errorf("members: %q: the sponsor covers %d GB of a %d GB allocation", mem.Name, mem.Sponsor.GB, *mem.AllocationGB)
			}
		} else if mem.AllocationGB != nil {
			ps.errorf("members: %q contributes disk, so allocation_gb is computed, not declared", mem.Name)
		}
		if mem.LegacyPGP != nil && len(mem.LegacyPGP.Fingerprint) != 40 {
			ps.errorf("members: %q: legacy_pgp.fingerprint must be the full 40-character fingerprint", mem.Name)
		}
	}
	for _, mem := range m.Members {
		if mem.Sponsor != nil && mem.Sponsor.Member != "reserve" && members[mem.Sponsor.Member] == nil {
			ps.errorf("members: %q: sponsor %q is not a member", mem.Name, mem.Sponsor.Member)
		}
		for _, v := range mem.VouchedBy {
			inviter := members[v.Member]
			if inviter == nil {
				ps.errorf("members: %q: vouched by %q, who is not a member", mem.Name, v.Member)
				continue
			}
			if err := v.Verify(m.Network.Name, inviter, &mem); err != nil {
				ps.errorf("members: %q: vouch by %q: %v", mem.Name, v.Member, err)
			}
		}
	}

	servers := 0
	byMember := map[string]int{}
	largest := 0
	for _, s := range m.Storage {
		if !nameRe.MatchString(s.Name) || seenNames["storage:"+s.Name] {
			ps.errorf("storage: name %q is invalid or duplicated", s.Name)
		}
		seenNames["storage:"+s.Name] = true
		if members[s.Member] == nil {
			ps.errorf("storage: %q belongs to %q, who is not a member", s.Name, s.Member)
		}
		if who, dup := seenKeys[s.Device]; dup {
			ps.errorf("storage: device key of %q already used by %s", s.Name, who)
		}
		seenKeys[s.Device] = "storage device " + s.Name
		if s.UDPPort != 0 && (s.UDPPort < 1024 || s.UDPPort > 65535) {
			ps.errorf("storage: %q: udp_port must be between 1024 and 65535", s.Name)
		}
		servers++
		byMember[s.Member]++
	}
	for _, mem := range m.Members {
		if mem.ContributionGB > 0 && byMember[mem.Name] == 0 {
			ps.errorf("members: %q contributes %d GB but lists no storage device", mem.Name, mem.ContributionGB)
		}
		if mem.ContributionGB > largest {
			largest = mem.ContributionGB
		}
	}
	if servers > 0 && n.Encoding.K > servers {
		ps.errorf("network.encoding: k=%d but only %d storage devices are listed; objects could never be read", n.Encoding.K, servers)
	}
	if servers > 0 && n.Encoding.N > servers {
		ps.warnf("network.encoding: n=%d but only %d storage devices are listed; shares will double up until more join", n.Encoding.N, servers)
	}

	// Capacity, in GB of shares (allocation times n/k): what contributing
	// members and member-sponsored clients use must fit in the contributions
	// less the reserve; what reserve-sponsored clients use comes out of the
	// reserve; and the reserve should still cover the largest server.
	total := 0
	for _, mem := range m.Members {
		total += mem.ContributionGB
	}
	if total > 0 && n.Encoding.K > 0 {
		reserve := total * n.ReservePercent / 100
		usable := total - reserve
		need, fromReserve := 0, 0
		for _, mem := range m.Members {
			shares := m.Allocation(mem.Name) * n.Encoding.N / n.Encoding.K
			if mem.ContributionGB == 0 && mem.Sponsor != nil && mem.Sponsor.Member == "reserve" {
				fromReserve += shares
			} else {
				need += shares
			}
		}
		if need+fromReserve > total {
			ps.errorf("allocations need %d GB of shares and the network contributes %d GB", need+fromReserve, total)
		} else {
			if need > usable {
				ps.warnf("allocations need %d GB of shares, more than the %d GB left after the %d%% reserve", need, usable, n.ReservePercent)
			}
			if fromReserve > reserve {
				ps.warnf("reserve-sponsored allocations need %d GB of shares, more than the %d GB reserve", fromReserve, reserve)
			}
		}
		if largest > reserve-fromReserve {
			ps.warnf("the reserve left after sponsorships (%d GB) does not cover the largest contribution (%d GB): a dead server could not be fully repaired", reserve-fromReserve, largest)
		}
	}
	return ps
}

// Allocation is the member's allocation in GB: contribution less the
// reserve, times k/n, or the declared allocation of a client-only member.
func (m *Manifest) Allocation(name string) int {
	for _, mem := range m.Members {
		if mem.Name != name {
			continue
		}
		if mem.ContributionGB == 0 {
			if mem.AllocationGB != nil {
				return *mem.AllocationGB
			}
			return 0
		}
		e := m.Network.Encoding
		if e.N == 0 {
			return 0
		}
		return mem.ContributionGB * (100 - m.Network.ReservePercent) / 100 * e.K / e.N
	}
	return 0
}

// MemberByKey finds a member by signing key.
func (m *Manifest) MemberByKey(k identity.Key) *Member {
	for i := range m.Members {
		if m.Members[i].Key == k {
			return &m.Members[i]
		}
	}
	return nil
}

// MemberByName finds a member by name.
func (m *Manifest) MemberByName(name string) *Member {
	for i := range m.Members {
		if m.Members[i].Name == name {
			return &m.Members[i]
		}
	}
	return nil
}

// HubByKey finds a hub by device key.
func (m *Manifest) HubByKey(k identity.Key) *Hub {
	for i := range m.Hubs {
		if m.Hubs[i].Key == k {
			return &m.Hubs[i]
		}
	}
	return nil
}

// ParseDate reads the manifest's date form.
func ParseDate(s string) (time.Time, error) { return time.Parse("2006-01-02", s) }
