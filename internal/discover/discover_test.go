// SPDX-License-Identifier: Apache-2.0

package discover

import (
	"testing"
	"time"

	"golang.org/x/net/dns/dnsmessage"

	v1 "github.com/spectrum-labs-tech/hospitium-protocol/v1"
)

// response builds an mDNS answer the way a TV sends one.
type response struct {
	t *testing.T
	b dnsmessage.Builder
}

func newResponse(t *testing.T) *response {
	t.Helper()
	r := &response{t: t, b: dnsmessage.NewBuilder(nil, dnsmessage.Header{Response: true, Authoritative: true})}
	if err := r.b.StartAnswers(); err != nil {
		t.Fatal(err)
	}
	return r
}

func hdr(name string) dnsmessage.ResourceHeader {
	return dnsmessage.ResourceHeader{Name: dnsmessage.MustNewName(name), Class: dnsmessage.ClassINET, TTL: 120}
}

func (r *response) ptr(service, instance string) *response {
	r.check(r.b.PTRResource(hdr(service), dnsmessage.PTRResource{PTR: dnsmessage.MustNewName(instance)}))
	return r
}

func (r *response) srv(instance, target string, port uint16) *response {
	r.check(r.b.SRVResource(hdr(instance), dnsmessage.SRVResource{Target: dnsmessage.MustNewName(target), Port: port}))
	return r
}

func (r *response) txt(instance string, kv ...string) *response {
	r.check(r.b.TXTResource(hdr(instance), dnsmessage.TXTResource{TXT: kv}))
	return r
}

func (r *response) a(host string, ip [4]byte) *response {
	r.check(r.b.AResource(hdr(host), dnsmessage.AResource{A: ip}))
	return r
}

func (r *response) check(err error) {
	r.t.Helper()
	if err != nil {
		r.t.Fatal(err)
	}
}

func (r *response) bytes() []byte {
	b, err := r.b.Finish()
	r.check(err)
	return b
}

func TestMergesWhatTVsAdvertise(t *testing.T) {
	t.Parallel()
	r := newRecords()
	// A Google TV advertising Cast and wireless debugging: one TV.
	must(t, r.add(newResponse(t).
		ptr("_googlecast._tcp.local.", "Google-TV-1a2b._googlecast._tcp.local.").
		srv("Google-TV-1a2b._googlecast._tcp.local.", "1a2b.local.", 8009).
		txt("Google-TV-1a2b._googlecast._tcp.local.", "id=1a2b3c", "fn=Living Room TV", "md=Chromecast HD", "ca=465413").
		a("1a2b.local.", [4]byte{10, 0, 50, 30}).bytes()))
	must(t, r.add(newResponse(t).
		ptr("_adb-tls-connect._tcp.local.", "adb-SERIAL1-x7Yz._adb-tls-connect._tcp.local.").
		srv("adb-SERIAL1-x7Yz._adb-tls-connect._tcp.local.", "Android.local.", 37099).
		a("Android.local.", [4]byte{10, 0, 50, 30}).bytes()))
	// A speaker: Cast without video out.
	must(t, r.add(newResponse(t).
		ptr("_googlecast._tcp.local.", "Nest-Mini._googlecast._tcp.local.").
		srv("Nest-Mini._googlecast._tcp.local.", "nest.local.", 8009).
		txt("Nest-Mini._googlecast._tcp.local.", "id=speaker", "fn=Kitchen speaker", "ca=199172").
		a("nest.local.", [4]byte{10, 0, 50, 40}).bytes()))
	// A TV only offering wireless debugging pairing.
	must(t, r.add(newResponse(t).
		ptr("_adb-tls-pairing._tcp.local.", "adb-SERIAL2._adb-tls-pairing._tcp.local.").
		srv("adb-SERIAL2._adb-tls-pairing._tcp.local.", "tv2.local.", 41000).
		a("tv2.local.", [4]byte{10, 0, 50, 31}).bytes()))

	now := time.Date(2026, 11, 16, 3, 0, 0, 0, time.UTC)
	tvs := r.tvs(now)
	if len(tvs) != 2 {
		t.Fatalf("tvs: %+v", tvs)
	}
	lr := tvs[0]
	if lr.Key != "cast:1a2b3c" || lr.Name != "Living Room TV" || lr.Model != "Chromecast HD" || lr.Address != "10.0.50.30" ||
		lr.ADBPort != 37099 || len(lr.Services) != 2 || !lr.SeenAt.Equal(now) {
		t.Fatalf("living room: %+v", lr)
	}
	if p := tvs[1]; p.Key != "adb:adb-serial2" || p.Address != "10.0.50.31" || p.ADBPort != 0 || p.Services[0] != v1.ServiceADBPairing {
		t.Fatalf("pairing-only TV: %+v", p)
	}
}

func TestAsksAgainForWhatAnswersLeftOut(t *testing.T) {
	t.Parallel()
	r := newRecords()
	must(t, r.add(newResponse(t).
		ptr("_googlecast._tcp.local.", "TV-A._googlecast._tcp.local.").
		ptr("_googlecast._tcp.local.", "TV-B._googlecast._tcp.local.").
		srv("TV-B._googlecast._tcp.local.", "tvb.local.", 8009).bytes()))
	qs := r.missing()
	want := map[string]dnsmessage.Type{}
	for _, q := range qs {
		want[q.Name.String()+" "+q.Type.String()] = q.Type
	}
	for _, k := range []string{"tv-a._googlecast._tcp.local. TypeSRV", "tv-a._googlecast._tcp.local. TypeTXT", "tvb.local. TypeA"} {
		if _, ok := want[k]; !ok {
			t.Errorf("missing question %s in %v", k, want)
		}
	}
	if len(r.tvs(time.Now())) != 0 {
		t.Fatal("TVs without addresses can't be proposed")
	}
}

func TestIgnoresQueriesAndGarbage(t *testing.T) {
	t.Parallel()
	r := newRecords()
	if err := r.add([]byte{1, 2, 3}); err == nil {
		t.Fatal("garbage should not parse")
	}
	q := dnsmessage.NewBuilder(nil, dnsmessage.Header{})
	must(t, q.StartQuestions())
	must(t, q.Question(question("_googlecast._tcp.local.", dnsmessage.TypePTR)))
	b, err := q.Finish()
	must(t, err)
	must(t, r.add(b))
	if len(r.ptr) != 0 {
		t.Fatal("another host's query is not an answer")
	}
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
