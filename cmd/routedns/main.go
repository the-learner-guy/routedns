package main

import (
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	rdns "github.com/folbricht/routedns"
	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
)

type options struct {
	logLevel uint32
}

func main() {
	var opt options
	cmd := &cobra.Command{
		Use:   "routedns <config> [<config>..]",
		Short: "DNS stub resolver, proxy and router",
		Long: `DNS stub resolver, proxy and router.

Listens for incoming DNS requests, routes, modifies and 
forwards to upstream resolvers. Supports plain DNS over 
UDP and TCP as well as DNS-over-TLS and DNS-over-HTTPS
as listener and client protocols.

Routes can be defined to send requests for certain queries;
by record type, query name or client-IP to different modifiers
or upstream resolvers.

Configuration can be split over multiple files with listeners,
groups and routers defined in different files and provided as
arguments.
`,
		Example: `  routedns config.toml`,
		Args:    cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return start(opt, args)
		},
		SilenceUsage: true,
	}
	cmd.Flags().Uint32VarP(&opt.logLevel, "log-level", "l", 4, "log level; 0=None .. 6=Trace")
	if err := cmd.Execute(); err != nil {
		os.Exit(1)
	}

}

func start(opt options, args []string) error {
	// Set the log level in the library package
	if opt.logLevel > 6 {
		return fmt.Errorf("invalid log level: %d", opt.logLevel)
	}
	rdns.Log.SetLevel(logrus.Level(opt.logLevel))

	config, err := loadConfig(args...)
	if err != nil {
		return err
	}

	// Map to hold all the resolvers extracted from the config, key'ed by resolver ID. It
	// holds configured resolvers, groups, as well as routers (since they all implement
	// rdns.Resolver)
	resolvers := make(map[string]rdns.Resolver)

	// Parse resolver config from the config first since groups and routers reference them
	for id, r := range config.Resolvers {
		if _, ok := resolvers[id]; ok {
			return fmt.Errorf("group resolver with duplicate id '%s", id)
		}
		switch r.Protocol {
		case "dot":
			tlsConfig, err := rdns.TLSClientConfig(r.CA, r.ClientCrt, r.ClientKey)
			if err != nil {
				return err
			}
			opt := rdns.DoTClientOptions{
				BootstrapAddr: r.BootstrapAddr,
				TLSConfig:     tlsConfig,
			}
			resolvers[id], err = rdns.NewDoTClient(r.Address, opt)
			if err != nil {
				return fmt.Errorf("failed to parse resolver config for '%s' : %s", id, err)
			}
		case "doh":
			tlsConfig, err := rdns.TLSClientConfig(r.CA, r.ClientCrt, r.ClientKey)
			if err != nil {
				return err
			}
			opt := rdns.DoHClientOptions{
				Method:        r.DoH.Method,
				TLSConfig:     tlsConfig,
				BootstrapAddr: r.BootstrapAddr,
			}
			resolvers[id], err = rdns.NewDoHClient(r.Address, opt)
			if err != nil {
				return fmt.Errorf("failed to parse resolver config for '%s' : %s", id, err)
			}
		case "tcp":
			resolvers[id] = rdns.NewDNSClient(r.Address, "tcp")
		case "udp":
			resolvers[id] = rdns.NewDNSClient(r.Address, "udp")
		default:
			return fmt.Errorf("unsupported protocol '%s' for resolver '%s'", r.Protocol, id)
		}
	}

	// Since routers depend on groups and vice-versa, build a map of IDs that reference a list of
	// IDs as dependencies. If all dependencies of an ID have been resolved (exist in the map of
	// resolvers), this entity can be instantiated in the following step.
	deps := make(map[string][]string)
	for id, g := range config.Groups {
		_, ok := deps[id]
		if ok {
			return fmt.Errorf("duplicate name: %s", id)
		}
		deps[id] = g.Resolvers
	}
	for id, r := range config.Routers {
		_, ok := deps[id]
		if ok {
			return fmt.Errorf("duplicate name: %s", id)
		}
		for _, route := range r.Routes {
			deps[id] = append(deps[id], route.Resolver)
		}
	}

	// Repeatedly iterate over the map, checking if all dependencies are resolveable. If
	// one is found, this node is instantiated and removed from the map. If nothing is found
	// during an iteration, the dependencies are missing and we bail out.
	for len(deps) > 0 {
		found := false
	node:
		for id, dependencies := range deps {
			for _, depID := range dependencies {
				if _, ok := resolvers[depID]; !ok {
					continue node
				}
			}
			// We found the ID of a node that can be instantiated. It could be a group
			// or a router. Try them both.
			if g, ok := config.Groups[id]; ok {
				if err := instantiateGroup(id, g, resolvers); err != nil {
					return err
				}
			}
			if r, ok := config.Routers[id]; ok {
				if err := instantiateRouter(id, r, resolvers); err != nil {
					return err
				}
			}
			delete(deps, id)
			found = true
		}
		if !found {
			return errors.New("unable to resolve dependencies")
		}
	}

	// Build the Listeners last as they can point to routers, groups or resolvers directly.
	var listeners []rdns.Listener
	for id, l := range config.Listeners {
		resolver, ok := resolvers[l.Resolver]
		if !ok {
			return fmt.Errorf("listener '%s' references non-existant resolver, group or router '%s", id, l.Resolver)
		}

		allowedNet, err := parseCIDRList(l.AllowedNet)
		if err != nil {
			return err
		}

		opt := rdns.ListenOptions{AllowedNet: allowedNet}

		switch l.Protocol {
		case "tcp":
			listeners = append(listeners, rdns.NewDNSListener(l.Address, "tcp", opt, resolver))
		case "udp":
			listeners = append(listeners, rdns.NewDNSListener(l.Address, "udp", opt, resolver))
		case "dot":
			tlsConfig, err := rdns.TLSServerConfig(l.CA, l.ServerCrt, l.ServerKey, l.MutualTLS)
			if err != nil {
				return err
			}
			ln := rdns.NewDoTListener(l.Address, rdns.DoTListenerOptions{TLSConfig: tlsConfig, ListenOptions: opt}, resolver)
			listeners = append(listeners, ln)
		case "doh":
			tlsConfig, err := rdns.TLSServerConfig(l.CA, l.ServerCrt, l.ServerKey, l.MutualTLS)
			if err != nil {
				return err
			}
			ln := rdns.NewDoHListener(l.Address, rdns.DoHListenerOptions{TLSConfig: tlsConfig, ListenOptions: opt}, resolver)
			listeners = append(listeners, ln)
		default:
			return fmt.Errorf("unsupported protocol '%s' for listener '%s'", l.Protocol, id)
		}
	}

	// Start the listeners
	for _, l := range listeners {
		go func(l rdns.Listener) {
			for {
				err := l.Start()
				rdns.Log.WithError(err).Error("listener failed")
				time.Sleep(time.Second)
			}
		}(l)
	}

	select {}
}

// Instantiate a group object based on configuration and add to the map of resolvers by ID.
func instantiateGroup(id string, g group, resolvers map[string]rdns.Resolver) error {
	var gr []rdns.Resolver
	var err error
	for _, rid := range g.Resolvers {
		resolver, ok := resolvers[rid]
		if !ok {
			return fmt.Errorf("group '%s' references non-existant resolver or group '%s", id, rid)
		}
		gr = append(gr, resolver)
	}
	switch g.Type {
	case "round-robin":
		resolvers[id] = rdns.NewRoundRobin(gr...)
	case "fail-rotate":
		resolvers[id] = rdns.NewFailRotate(gr...)
	case "fail-back":
		resolvers[id] = rdns.NewFailBack(rdns.FailBackOptions{ResetAfter: time.Minute}, gr...)
	case "blocklist":
		if len(gr) != 1 {
			return fmt.Errorf("type blocklist only supports one resolver in '%s'", id)
		}
		if len(g.Blocklist) > 0 && g.Source != "" {
			return fmt.Errorf("static blocklist can't be used with 'source' in '%s'", id)
		}
		if len(g.Allowlist) > 0 && g.AllowlistSource != "" {
			return fmt.Errorf("static allowlist can't be used with 'source' in '%s'", id)
		}
		var blocklistLoader rdns.BlocklistLoader
		if g.Source != "" {
			loc, err := url.Parse(g.Source)
			if err != nil {
				return err
			}
			switch loc.Scheme {
			case "http", "https":
				blocklistLoader = rdns.NewHTTPLoader(g.Source)
			case "":
				blocklistLoader = rdns.NewFileLoader(g.Source)
			default:
				return fmt.Errorf("unsupported scheme '%s' in '%s'", loc.Scheme, g.Source)
			}
		}
		var allowlistLoader rdns.BlocklistLoader
		if g.AllowlistSource != "" {
			loc, err := url.Parse(g.AllowlistSource)
			if err != nil {
				return err
			}
			switch loc.Scheme {
			case "http", "https":
				allowlistLoader = rdns.NewHTTPLoader(g.AllowlistSource)
			case "":
				allowlistLoader = rdns.NewFileLoader(g.AllowlistSource)
			default:
				return fmt.Errorf("unsupported scheme '%s' in '%s'", loc.Scheme, g.AllowlistSource)
			}
		}
		var blocklistDB rdns.BlocklistDB
		switch g.Format {
		case "regexp", "":
			blocklistDB, err = rdns.NewRegexpDB(g.Blocklist...)
			if err != nil {
				return err
			}
		case "domain":
			blocklistDB, err = rdns.NewDomainDB(g.Blocklist...)
			if err != nil {
				return err
			}
		case "hosts":
			blocklistDB, err = rdns.NewHostsDB(g.Blocklist...)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported blocklist format '%s'", g.Format)
		}
		var allowlistDB rdns.BlocklistDB
		switch g.AllowlistFormat {
		case "regexp", "":
			allowlistDB, err = rdns.NewRegexpDB(g.Allowlist...)
			if err != nil {
				return err
			}
		case "domain":
			allowlistDB, err = rdns.NewDomainDB(g.Allowlist...)
			if err != nil {
				return err
			}
		case "hosts":
			allowlistDB, err = rdns.NewHostsDB(g.Allowlist...)
			if err != nil {
				return err
			}
		default:
			return fmt.Errorf("unsupported allowlist format '%s'", g.Format)
		}
		opt := rdns.BlocklistOptions{
			BlocklistDB:      blocklistDB,
			BlocklistLoader:  blocklistLoader,
			BlocklistRefresh: time.Duration(g.Refresh) * time.Second,
			AllowlistDB:      allowlistDB,
			AllowlistLoader:  allowlistLoader,
			AllowlistRefresh: time.Duration(g.AllowlistRefresh) * time.Second,
		}
		resolvers[id], err = rdns.NewBlocklist(gr[0], opt)
		if err != nil {
			return err
		}
	case "replace":
		if len(gr) != 1 {
			return fmt.Errorf("type replace only supports one resolver in '%s'", id)
		}
		resolvers[id], err = rdns.NewReplace(gr[0], g.Replace...)
		if err != nil {
			return err
		}
	case "ecs-modifier":
		if len(gr) != 1 {
			return fmt.Errorf("type ecs-modifier only supports one resolver in '%s'", id)
		}
		var f rdns.ECSModifierFunc
		switch g.ECSOp {
		case "add":
			f = rdns.ECSModifierAdd(g.ECSAddress, g.ECSPrefix4, g.ECSPrefix6)
		case "delete":
			f = rdns.ECSModifierDelete
		case "privacy":
			f = rdns.ECSModifierPrivacy(g.ECSPrefix4, g.ECSPrefix6)
		case "":
		default:
			return fmt.Errorf("unsupported ecs-modifier operation '%s'", g.ECSOp)
		}
		resolvers[id], err = rdns.NewECSModifier(gr[0], f)
		if err != nil {
			return err
		}
	case "cache":
		resolvers[id] = rdns.NewCache(gr[0], time.Duration(g.GCPeriod)*time.Second)
	default:
		return fmt.Errorf("unsupported group type '%s' for group '%s", g.Type, id)
	}
	return nil
}

// Instantiate a router object based on configuration and add to the map of resolvers by ID.
func instantiateRouter(id string, r router, resolvers map[string]rdns.Resolver) error {
	router := rdns.NewRouter()
	for _, route := range r.Routes {
		resolver, ok := resolvers[route.Resolver]
		if !ok {
			return fmt.Errorf("router '%s' references non-existant resolver or group '%s'", id, route.Resolver)
		}
		if err := router.Add(route.Name, route.Type, route.Source, resolver); err != nil {
			return fmt.Errorf("failure parsing routes for router '%s' : %s", id, err.Error())
		}
	}
	resolvers[id] = router
	return nil
}
