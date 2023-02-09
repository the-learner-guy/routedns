package rdns

import (
	"sync"
	"time"

	"github.com/miekg/dns"
	"github.com/sirupsen/logrus"
)

// ClientBlocklist is a resolver that matches the IPs of clients against a blocklist
type ClientBlocklist struct {
	id string
	ClientBlocklistOptions
	resolver Resolver
	mu       sync.RWMutex
	metrics  *BlocklistMetrics
}

var _ Resolver = &ClientBlocklist{}

type ClientBlocklistOptions struct {
	// Optional, if the client is found to match the blocklist, send the query to this resolver.
	BlocklistResolver Resolver

	BlocklistDB IPBlocklistDB

	// Refresh period for the blocklist. Disabled if 0.
	BlocklistRefresh time.Duration
}

// NewClientBlocklistIP returns a new instance of a client blocklist resolver.
func NewClientBlocklist(id string, resolver Resolver, opt ClientBlocklistOptions) (*ClientBlocklist, error) {
	blocklist := &ClientBlocklist{
		id:                     id,
		resolver:               resolver,
		ClientBlocklistOptions: opt,
		metrics:                NewBlocklistMetrics(id),
	}

	// Start the refresh goroutines if we have a list and a refresh period was given
	if blocklist.BlocklistDB != nil && blocklist.BlocklistRefresh > 0 {
		go blocklist.refreshLoopBlocklist(blocklist.BlocklistRefresh)
	}
	return blocklist, nil
}

// Resolve a DNS query after checking the client's IP against a blocklist. Responds with
// REFUSED if the client IP is on the blocklist, or sends the query to an alternative
// resolver if one is configured.
func (r *ClientBlocklist) Resolve(q *dns.Msg, ci ClientInfo) (*dns.Msg, error) {
	if match, ok := r.BlocklistDB.Match(ci.SourceIP); ok {
		log := Log.WithFields(logrus.Fields{"id": r.id, "qname": qName(q), "list": match.List, "rule": match.Rule, "ip": ci.SourceIP})
		r.metrics.blocked.Add(1)
		if r.BlocklistResolver != nil {
			log.WithField("resolver", r.BlocklistResolver).Debug("client on blocklist, forwarding to blocklist-resolver")
			return r.BlocklistResolver.Resolve(q, ci)
		}
		log.Debug("blocking client")
		return refused(q), nil
	}

	r.metrics.allowed.Add(1)
	return r.resolver.Resolve(q, ci)
}

func (r *ClientBlocklist) String() string {
	return r.id
}

func (r *ClientBlocklist) refreshLoopBlocklist(refresh time.Duration) {
	for {
		time.Sleep(refresh)
		log := Log.WithField("id", r.id)
		log.Debug("reloading blocklist")
		db, err := r.BlocklistDB.Reload()
		if err != nil {
			Log.WithError(err).Error("failed to load rules")
			continue
		}
		r.mu.Lock()
		r.BlocklistDB.Close()
		r.BlocklistDB = db
		r.mu.Unlock()
	}
}
