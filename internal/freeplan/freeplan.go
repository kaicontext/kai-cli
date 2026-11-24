// Package freeplan reads the caller's plan from the model list (GET
// /api/v1/llm/models) and applies it to the models a command runs. The
// same package is in kai-tui (internal/freeplan); keep the two alike.
//
// For a caller on the Free plan the server may answer with free: true,
// only the models the Free plan includes, and the model for each of the
// apps' roles (defaults: chat, robot, planner, coder, agent, jeff,
// images, titles). A role the person didn't set runs the server's model.
// A model they did set that the Free plan doesn't include stops the
// command with one sentence saying so (Check). Everyone else gets the list
// without a free key, and nothing here changes anything.
package freeplan

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/kaicontext/kai-engine/clientid"
	"github.com/kaicontext/kai-engine/modelplan"
)

// CatchAll is the server role a model is resolved for when the command's
// role has no key of its own in defaults: the commit review and its
// stages, the classifier, the critic and the consult tool.
const CatchAll = "agent"

// Plan is what the model list said. The zero value is "not Free".
type Plan struct {
	Free     bool
	Defaults map[string]string
	allowed  map[string]bool
	server   string
}

// fetchTimeout bounds the one extra request a command makes; a slow or
// missing answer is "not Free", which is today's behaviour.
const fetchTimeout = 3 * time.Second

// Fetch asks server for the caller's model list. Any failure is the zero
// Plan.
func Fetch(ctx context.Context, server, token string) Plan {
	server = strings.TrimRight(strings.TrimSpace(server), "/")
	if server == "" || strings.TrimSpace(token) == "" {
		return Plan{}
	}
	ctx, cancel := context.WithTimeout(ctx, fetchTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, server+"/api/v1/llm/models", nil)
	if err != nil {
		return Plan{}
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := clientid.Client(&http.Client{Timeout: fetchTimeout}).Do(req)
	if err != nil {
		return Plan{}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Plan{}
	}
	var body struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
		Free     bool              `json:"free"`
		Defaults map[string]string `json:"defaults"`
	}
	if json.NewDecoder(resp.Body).Decode(&body) != nil || !body.Free {
		return Plan{}
	}
	p := Plan{Free: true, Defaults: map[string]string{}, allowed: map[string]bool{}, server: server}
	for _, m := range body.Data {
		p.allowed[strings.TrimSpace(m.ID)] = true
	}
	for role, m := range body.Defaults {
		if m = strings.TrimSpace(m); m != "" {
			p.Defaults[role] = m
			p.allowed[m] = true
		}
	}
	return p
}

var (
	currentOnce sync.Once
	current     Plan
)

// Current is Fetch, once per process: every role a command resolves sees
// the same answer, and the list is asked for at most once.
func Current(server, token string) Plan {
	currentOnce.Do(func() { current = Fetch(context.Background(), server, token) })
	return current
}

// Engine is p as the engine takes it (modelplan.Plan), for the app to hand
// over with modelplan.Set so the models the engine picks on its own follow
// the plan too: its client default, a run's rescue model, the
// stronger-model retry and the screenshot check's strong lane. The zero
// plan off Free (the engine keeps its own models), else Free with the role
// defaults, every model the list allows and the upgrade link.
func (p Plan) Engine() modelplan.Plan {
	if !p.Free {
		return modelplan.Plan{}
	}
	defaults := make(map[string]string, len(p.Defaults))
	for role, m := range p.Defaults {
		defaults[role] = m
	}
	allowed := make([]string, 0, len(p.allowed))
	for m := range p.allowed {
		if m != "" {
			allowed = append(allowed, m)
		}
	}
	sort.Strings(allowed)
	billing := ""
	if p.server != "" {
		billing = p.BillingURL()
	}
	return modelplan.Plan{Free: true, Defaults: defaults, Allowed: allowed, BillingURL: billing}
}

// Allows reports whether model may run: always off Free, else when the
// list offers it or names it as a role's default (our ":reasoning"
// variant counts as its base model, as on the server).
func (p Plan) Allows(model string) bool {
	if !p.Free {
		return true
	}
	m := strings.TrimSpace(model)
	if p.allowed[m] {
		return true
	}
	base, _, _ := strings.Cut(m, ":")
	return p.allowed[base]
}

// Default is the server's model for role, falling back to CatchAll's for a
// role it names none for; "" off Free.
func (p Plan) Default(role string) string {
	if !p.Free {
		return ""
	}
	if m := p.Defaults[role]; m != "" {
		return m
	}
	return p.Defaults[CatchAll]
}

// Resolve is the model a role runs: what the person set (explicit) wins,
// else on Free the server's model for the role, else fallback.
func (p Plan) Resolve(role, explicit, fallback string) string {
	if e := strings.TrimSpace(explicit); e != "" {
		return e
	}
	if m := p.Default(role); m != "" {
		return m
	}
	return fallback
}

// BillingURL is where the plan is upgraded.
func (p Plan) BillingURL() string {
	if p.server == "" {
		return "the Billing page of your Kai dashboard"
	}
	return p.server + "/billing"
}

// Check is the error a command stops with when the person set a model the
// Free plan doesn't include: the first such model in explicit, in order.
// Nil off Free, or when every model is included.
func (p Plan) Check(explicit []string) error {
	if !p.Free {
		return nil
	}
	for _, m := range explicit {
		if m = strings.TrimSpace(m); m != "" && !p.Allows(m) {
			return fmt.Errorf("%s isn't available on the Free plan. Choose another model, or upgrade: %s.", m, p.BillingURL())
		}
	}
	return nil
}
