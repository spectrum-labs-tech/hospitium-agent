// SPDX-License-Identifier: Apache-2.0

// Package discover finds TVs on the hub's network over mDNS, so the control plane can propose them
// to the owner instead of the owner typing IP addresses. It sends one-shot queries from an
// ephemeral port (RFC 6762 §5.1), so it needs no privileges and coexists with avahi on 5353, and it
// never answers queries itself.
package discover

import (
	"context"
	"errors"
	"net"
	"slices"
	"strconv"
	"strings"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"
)

var services = []string{v1.ServiceCast, v1.ServiceADBConnect, v1.ServiceADBPairing}

var mdnsGroup = &net.UDPAddr{IP: net.IPv4(224, 0, 0, 251), Port: 5353}

// Browse queries for TVs, listens for wait, asks again for anything answers left out, and returns
// what it found, at most v1.MaxDiscovered.
func Browse(ctx context.Context, wait time.Duration) ([]v1.DiscoveredTV, error) {
	var lc net.ListenConfig
	conn, err := lc.ListenPacket(ctx, "udp4", ":0")
	if err != nil {
		return nil, err
	}
	defer conn.Close()
	stop := context.AfterFunc(ctx, func() { _ = conn.Close() })
	defer stop()

	r := newRecords()
	qs := make([]dnsmessage.Question, 0, len(services))
	for _, s := range services {
		qs = append(qs, question(s+".local.", dnsmessage.TypePTR))
	}
	if err := send(conn, qs); err != nil {
		return nil, err
	}
	collect(conn, r, wait)
	if more := r.missing(); len(more) > 0 && ctx.Err() == nil {
		if err := send(conn, more); err != nil {
			return nil, err
		}
		collect(conn, r, wait/2)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return r.tvs(time.Now()), nil
}

func question(name string, t dnsmessage.Type) dnsmessage.Question {
	return dnsmessage.Question{Name: dnsmessage.MustNewName(name), Type: t, Class: dnsmessage.ClassINET}
}

func send(conn net.PacketConn, qs []dnsmessage.Question) error {
	b := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	b.EnableCompression()
	if err := b.StartQuestions(); err != nil {
		return err
	}
	for _, q := range qs {
		if err := b.Question(q); err != nil {
			return err
		}
	}
	msg, err := b.Finish()
	if err != nil {
		return err
	}
	_, err = conn.WriteTo(msg, mdnsGroup)
	return err
}

// collect reads answers until wait elapses. Packets that don't parse are skipped: one odd device
// must not hide the others.
func collect(conn net.PacketConn, r *records, wait time.Duration) {
	_ = conn.SetReadDeadline(time.Now().Add(wait))
	buf := make([]byte, 9000)
	for {
		n, _, err := conn.ReadFrom(buf)
		if err != nil {
			return
		}
		_ = r.add(buf[:n])
	}
}

type srv struct {
	target string
	port   int
}

// records accumulates what responders said, keyed by lower-cased names (DNS names are
// case-insensitive).
type records struct {
	ptr map[string][]string
	srv map[string]srv
	txt map[string]map[string]string
	a   map[string]net.IP
}

func newRecords() *records {
	return &records{ptr: map[string][]string{}, srv: map[string]srv{}, txt: map[string]map[string]string{}, a: map[string]net.IP{}}
}

func (r *records) add(msg []byte) error {
	var p dnsmessage.Parser
	h, err := p.Start(msg)
	if err != nil {
		return err
	}
	if !h.Response {
		return nil
	}
	if err := p.SkipAllQuestions(); err != nil {
		return err
	}
	for _, next := range []func() (dnsmessage.ResourceHeader, error){p.AnswerHeader, p.AuthorityHeader, p.AdditionalHeader} {
		for {
			rh, err := next()
			if errors.Is(err, dnsmessage.ErrSectionDone) {
				break
			}
			if err != nil {
				return err
			}
			if err := r.resource(&p, rh); err != nil {
				return err
			}
		}
	}
	return nil
}

func (r *records) resource(p *dnsmessage.Parser, rh dnsmessage.ResourceHeader) error {
	name := strings.ToLower(rh.Name.String())
	switch rh.Type {
	case dnsmessage.TypePTR:
		res, err := p.PTRResource()
		if err != nil {
			return err
		}
		if inst := strings.ToLower(res.PTR.String()); !slices.Contains(r.ptr[name], inst) {
			r.ptr[name] = append(r.ptr[name], inst)
		}
	case dnsmessage.TypeSRV:
		res, err := p.SRVResource()
		if err != nil {
			return err
		}
		r.srv[name] = srv{target: strings.ToLower(res.Target.String()), port: int(res.Port)}
	case dnsmessage.TypeTXT:
		res, err := p.TXTResource()
		if err != nil {
			return err
		}
		kv := map[string]string{}
		for _, s := range res.TXT {
			k, v, _ := strings.Cut(s, "=")
			kv[strings.ToLower(k)] = v
		}
		r.txt[name] = kv
	case dnsmessage.TypeA:
		res, err := p.AResource()
		if err != nil {
			return err
		}
		r.a[name] = net.IP(res.A[:])
	default:
		_, err := p.UnknownResource()
		return err
	}
	return nil
}

// missing asks for what answers left out: an instance's SRV and TXT, or a host's address.
func (r *records) missing() []dnsmessage.Question {
	var qs []dnsmessage.Question
	for _, s := range services {
		for _, inst := range r.ptr[s+".local."] {
			sv, ok := r.srv[inst]
			if !ok {
				qs = append(qs, question(inst, dnsmessage.TypeSRV), question(inst, dnsmessage.TypeTXT))
				continue
			}
			if _, ok := r.a[sv.target]; !ok {
				qs = append(qs, question(sv.target, dnsmessage.TypeA))
			}
		}
	}
	return qs
}

// tvs merges what was learned into one entry per address: a TV advertising Cast and wireless
// debugging is one TV. Cast devices without video output (speakers) are left out.
func (r *records) tvs(now time.Time) []v1.DiscoveredTV {
	byIP := map[string]*v1.DiscoveredTV{}
	var order []string
	for _, s := range services {
		for _, inst := range r.ptr[s+".local."] {
			sv, ok := r.srv[inst]
			if !ok {
				continue
			}
			ip := r.a[sv.target]
			if ip == nil || ip.To4() == nil {
				continue
			}
			txt := r.txt[inst]
			if s == v1.ServiceCast && !hasVideo(txt) {
				continue
			}
			addr := ip.To4().String()
			tv := byIP[addr]
			if tv == nil {
				tv = &v1.DiscoveredTV{Address: addr, SeenAt: now}
				byIP[addr] = tv
				order = append(order, addr)
			}
			if !slices.Contains(tv.Services, s) {
				tv.Services = append(tv.Services, s)
			}
			label := strings.TrimSuffix(inst, "."+s+".local.")
			switch s {
			case v1.ServiceCast:
				tv.Key = "cast:" + label
				if id := txt["id"]; id != "" {
					tv.Key = "cast:" + id
				}
				tv.Name, tv.Model = txt["fn"], txt["md"]
			case v1.ServiceADBConnect:
				tv.ADBPort = sv.port
			}
			if tv.Key == "" {
				tv.Key = "adb:" + label
			}
		}
	}
	slices.Sort(order)
	out := make([]v1.DiscoveredTV, 0, min(len(order), v1.MaxDiscovered))
	for _, addr := range order {
		if len(out) == v1.MaxDiscovered {
			break
		}
		out = append(out, *byIP[addr])
	}
	return out
}

// hasVideo reads the Cast capability bitmask ("ca"); bit 0 is video out. A device that doesn't say
// is kept.
func hasVideo(txt map[string]string) bool {
	ca, ok := txt["ca"]
	if !ok {
		return true
	}
	n, err := strconv.Atoi(ca)
	return err != nil || n&1 == 1
}
