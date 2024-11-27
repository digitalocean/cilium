// SPDX-License-Identifier: Apache-2.0
// Copyright Authors of Cilium

package proxy

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"

	"github.com/cilium/cilium/api/v1/models"
	"github.com/cilium/cilium/pkg/completion"
	"github.com/cilium/cilium/pkg/lock"
	"github.com/cilium/cilium/pkg/logging"
	"github.com/cilium/cilium/pkg/logging/logfields"
	"github.com/cilium/cilium/pkg/metrics"
	"github.com/cilium/cilium/pkg/node"
	"github.com/cilium/cilium/pkg/option"
	"github.com/cilium/cilium/pkg/policy"
	"github.com/cilium/cilium/pkg/proxy/endpoint"
	"github.com/cilium/cilium/pkg/proxy/proxyports"
	"github.com/cilium/cilium/pkg/proxy/types"
	"github.com/cilium/cilium/pkg/revert"
)

var log = logging.DefaultLogger.WithField(logfields.LogSubsys, "proxy")

// field names used while logging
const (
	fieldProxyRedirectID = "id"

	// redirectCreationAttempts is the number of attempts to create a redirect
	redirectCreationAttempts = 5
)

// Proxy maintains state about redirects
type Proxy struct {
	// mutex is the lock required when modifying any proxy datastructure
	mutex lock.RWMutex

	// redirects is the map of all redirect configurations indexed by
	// the redirect identifier. Redirects may be implemented by different
	// proxies.
	redirects map[string]*Redirect

	envoyIntegration *envoyProxyIntegration
	dnsIntegration   *dnsProxyIntegration

	// proxyPorts manages proxy port allocation
	proxyPorts *proxyports.ProxyPorts
}

func createProxy(
	minPort uint16,
	maxPort uint16,
	datapathUpdater proxyports.DatapathUpdater,
	envoyIntegration *envoyProxyIntegration,
	dnsIntegration *dnsProxyIntegration,
) *Proxy {
	return &Proxy{
		redirects:        make(map[string]*Redirect),
		envoyIntegration: envoyIntegration,
		dnsIntegration:   dnsIntegration,
		proxyPorts:       proxyports.NewProxyPorts(minPort, maxPort, datapathUpdater),
	}
}

// AckProxyPort() marks the proxy of the given type as successfully
// created and creates or updates the datapath rules accordingly.
// Takes a reference on the proxy port.
func (p *Proxy) AckProxyPort(ctx context.Context, name string) error {
	return p.proxyPorts.AckProxyPortWithReference(ctx, name)
}

// AllocateCRDProxyPort() allocates a new port for listener 'name', or returns the current one if
// already allocated.
// Each call has to be paired with AckProxyPort(name) to update the datapath rules accordingly.
// Each allocated port must be eventually freed with ReleaseProxyPort().
func (p *Proxy) AllocateCRDProxyPort(name string) (uint16, error) {
	return p.proxyPorts.AllocateCRDProxyPort(name)
}

func (p *Proxy) ReleaseProxyPort(name string) error {
	return p.proxyPorts.ReleaseProxyPort(name)
}

func (p *Proxy) ReinstallRoutingRules(mtu int) error {
	return ReinstallRoutingRules(mtu)
}

// GetProxyPort() returns the fixed listen port for a proxy, if any.
func (p *Proxy) GetProxyPort(name string) (port uint16, isStatic bool, err error) {
	return p.proxyPorts.GetProxyPort(name)
}

// SetProxyPort() marks the proxy 'name' as successfully created with proxy port 'port'.
// Another call to AckProxyPort(name) is needed to update the datapath rules accordingly.
// This should only be called for proxies that have a static listener that is already listening on
// 'port'. May only be called once per proxy.

func (p *Proxy) SetProxyPort(name string, proxyType types.ProxyType, port uint16, ingress bool) error {
	return p.proxyPorts.SetProxyPort(name, proxyType, port, ingress)
}

// OpenLocalPorts returns the set of L4 ports currently open locally.
func OpenLocalPorts() map[uint16]struct{} {
	return proxyports.OpenLocalPorts()
}

// CreateOrUpdateRedirect creates or updates a L4 redirect with corresponding
// proxy configuration. This will allocate a proxy port as required and launch
// a proxy instance. If the redirect is already in place, only the rules will be
// updated.
// The proxy listening port is returned, but proxy configuration on that port
// may still be ongoing asynchronously. Caller should wait for successful completion
// on 'wg' before assuming the returned proxy port is listening.
// Caller must call exactly one of the returned functions:
// - finalizeFunc to make the changes stick, or
// - revertFunc to cancel the changes.
// Called with 'localEndpoint' locked!
func (p *Proxy) CreateOrUpdateRedirect(
	ctx context.Context, l4 policy.ProxyPolicy, id string, localEndpoint endpoint.EndpointUpdater, wg *completion.WaitGroup,
) (
	uint16, error, revert.FinalizeFunc, revert.RevertFunc,
) {
	p.mutex.Lock()
	defer p.mutex.Unlock()

	scopedLog := log.
		WithField(fieldProxyRedirectID, id).
		WithField(logfields.Listener, l4.GetListener()).
		WithField("l7parser", l4.GetL7Parser())

	var finalizeList revert.FinalizeList
	var revertStack revert.RevertStack

	// Check for existing redirect and try to update it if possible. Otherwise, it gets removed before re-creation.
	if existingRedirect, ok := p.redirects[id]; ok {
		existingRedirect.mutex.Lock()

		// Only consider configured (but not necessarily acked) proxy ports for update
		if p.proxyPorts.HasProxyType(existingRedirect.listener, types.ProxyType(l4.GetL7Parser())) {
			updateRevertFunc := existingRedirect.updateRules(l4)
			revertStack.Push(updateRevertFunc)
			implUpdateRevertFunc, err := existingRedirect.implementation.UpdateRules(wg)
			if err != nil {
				existingRedirect.mutex.Unlock()
				p.revertStackUnlocked(revertStack)
				return 0, fmt.Errorf("unable to update existing redirect: %w", err), nil, nil
			}

			revertStack.Push(implUpdateRevertFunc)

			scopedLog.
				WithField(logfields.Object, logfields.Repr(existingRedirect)).
				WithField("proxyType", existingRedirect.listener.ProxyType).
				Debug("updated existing proxy instance")

			existingRedirect.mutex.Unlock()

			// Must return the proxy port when successful
			return existingRedirect.listener.ProxyPort, nil, nil, revertStack.Revert
		}

		// Stale or incompatible redirects get removed before a new one is created below
		err, removeFinalizeFunc, removeRevertFunc := p.removeRedirect(id, wg)
		existingRedirect.mutex.Unlock()

		if err != nil {
			p.revertStackUnlocked(revertStack)
			return 0, fmt.Errorf("unable to remove old redirect: %w", err), nil, nil
		}

		finalizeList.Append(removeFinalizeFunc)
		revertStack.Push(removeRevertFunc)
	}

	// Create a new redirect
	port, err, newRedirectFinalizeFunc, newRedirectRevertFunc := p.createNewRedirect(ctx, l4, id, localEndpoint, wg)
	if err != nil {
		p.revertStackUnlocked(revertStack)
		return 0, fmt.Errorf("failed to create new redirect: %w", err), nil, nil
	}

	finalizeList.Append(newRedirectFinalizeFunc)
	revertStack.Push(newRedirectRevertFunc)

	return port, nil, finalizeList.Finalize, revertStack.Revert
}

func proxyTypeNotFoundError(proxyType types.ProxyType, listener string, ingress bool) error {
	dir := "egress"
	if ingress {
		dir = "ingress"
	}
	return fmt.Errorf("unrecognized %s proxy type for %s: %s", dir, listener, proxyType)
}

func (p *Proxy) createNewRedirect(
	ctx context.Context, l4 policy.ProxyPolicy, id string, localEndpoint endpoint.EndpointUpdater, wg *completion.WaitGroup,
) (
	uint16, error, revert.FinalizeFunc, revert.RevertFunc,
) {
	scopedLog := log.
		WithField(fieldProxyRedirectID, id).
		WithField(logfields.Listener, l4.GetListener()).
		WithField("l7parser", l4.GetL7Parser())

	ppName, pp := p.proxyPorts.FindByType(types.ProxyType(l4.GetL7Parser()), l4.GetListener(), l4.GetIngress())
	if pp == nil {
		return 0, proxyTypeNotFoundError(types.ProxyType(l4.GetL7Parser()), l4.GetListener(), l4.GetIngress()), nil, nil
	}

	redirect := newRedirect(localEndpoint, ppName, pp, l4.GetPort(), l4.GetProtocol())
	_ = redirect.updateRules(l4) // revertFunc not used because revert will remove whole redirect
	// Rely on create*Redirect to update rules, unlike the update case above.

	scopedLog = scopedLog.
		WithField("portName", ppName)

	// try first with the previous port, if any
	p.proxyPorts.Restore(pp)

	var err error
	for nRetry := 0; nRetry < redirectCreationAttempts; nRetry++ {
		if err != nil {
			// an error occurred and we are retrying
			scopedLog.
				WithError(err).
				WithField(logfields.ProxyPort, pp.ProxyPort).
				Warning("Unable to create proxy, retrying")
		}

		err = p.proxyPorts.AllocatePort(pp, nRetry > 0)
		if err != nil {
			err = fmt.Errorf("failed to allocate port: %w", err)
			break
		}

		err = p.createRedirectImpl(redirect, l4, wg)
		if err == nil {
			break
		}
	}

	if err != nil {
		// an error occurred, and we have no more retries
		scopedLog.
			WithError(err).
			WithField(logfields.ProxyPort, pp.ProxyPort).
			Error("Unable to create proxy")
		return 0, fmt.Errorf("failed to create redirect implementation: %w", err), nil, nil
	}

	scopedLog.
		WithField(logfields.Object, logfields.Repr(redirect)).
		WithField(logfields.ProxyPort, pp.ProxyPort).
		Info("Created new proxy instance")

	p.redirects[id] = redirect
	p.updateRedirectMetrics()

	revertFunc := func() error {
		// Proxy port refcount has not been incremented yet, so it must not be decremented
		// when reverting. Undo what we have done above.
		p.mutex.Lock()
		delete(p.redirects, id)

		// Does not release static proxy ports
		p.proxyPorts.Reset(pp)

		p.updateRedirectMetrics()
		p.mutex.Unlock()
		implFinalizeFunc, _ := redirect.implementation.Close(wg)
		if implFinalizeFunc != nil {
			implFinalizeFunc()
		}
		return nil
	}

	// Increase the reference count only when ACK is received
	finalizeFunc := func() {
		p.proxyPorts.AckProxyPortWithReference(ctx, ppName)
	}

	// Must return the proxy port when successful
	return pp.ProxyPort, nil, finalizeFunc, revertFunc
}

func (p *Proxy) createRedirectImpl(redir *Redirect, l4 policy.ProxyPolicy, wg *completion.WaitGroup) error {
	var err error

	switch l4.GetL7Parser() {
	case policy.ParserTypeDNS:
		redir.implementation, err = p.dnsIntegration.createRedirect(redir, wg)
		// 'cb' not called for DNS redirects, which have a static proxy port
	default:
		redir.implementation, err = p.envoyIntegration.createRedirect(redir, wg)
	}

	return err
}

func (p *Proxy) revertStackUnlocked(revertStack revert.RevertStack) {
	// We ignore errors while reverting. This is best-effort.
	// revertFunc must be called after p.mutex is unlocked, because
	// some functions in the revert stack (like removeRevertFunc)
	// require it
	p.mutex.Unlock()
	revertStack.Revert()
	p.mutex.Lock()
}

// RemoveRedirect removes an existing redirect that has been successfully created earlier.
func (p *Proxy) RemoveRedirect(id string, wg *completion.WaitGroup) (error, revert.FinalizeFunc, revert.RevertFunc) {
	p.mutex.Lock()
	defer func() {
		p.updateRedirectMetrics()
		p.mutex.Unlock()
	}()
	return p.removeRedirect(id, wg)
}

// removeRedirect removes an existing redirect. p.mutex must be held
// p.mutex must NOT be held when the returned revert function is called!
// proxyPortsMutex must NOT be held when the returned finalize function is called!
func (p *Proxy) removeRedirect(id string, wg *completion.WaitGroup) (error, revert.FinalizeFunc, revert.RevertFunc) {
	log.
		WithField(fieldProxyRedirectID, id).
		Debug("Removing proxy redirect")

	var finalizeList revert.FinalizeList
	var revertStack revert.RevertStack

	r, ok := p.redirects[id]
	if !ok {
		return fmt.Errorf("unable to find redirect %s", id), nil, nil
	}
	delete(p.redirects, id)

	implFinalizeFunc, implRevertFunc := r.implementation.Close(wg)

	finalizeList.Append(implFinalizeFunc)
	revertStack.Push(implRevertFunc)

	// Delay the release and reuse of the port number so it is guaranteed to be
	// safe to listen on the port again. This can't be reverted, so do it in a
	// FinalizeFunc.
	proxyPort := r.listener.ProxyPort
	listenerName := r.name

	finalizeList.Append(func() {
		// break GC loop (implementation may point back to 'r')
		r.implementation = nil

		err := p.proxyPorts.ReleaseProxyPort(listenerName)
		if err != nil {
			log.
				WithField(fieldProxyRedirectID, id).
				WithField("proxyPort", proxyPort).
				WithError(err).
				Warning("Releasing proxy port failed")
		}
	})

	revertStack.Push(func() error {
		p.mutex.Lock()
		p.redirects[id] = r
		p.mutex.Unlock()

		return nil
	})

	return nil, finalizeList.Finalize, revertStack.Revert
}

func (p *Proxy) UpdateNetworkPolicy(ep endpoint.EndpointUpdater, vis *policy.VisibilityPolicy, policy *policy.L4Policy, ingressPolicyEnforced, egressPolicyEnforced bool, wg *completion.WaitGroup) (error, func() error) {
	return p.envoyIntegration.UpdateNetworkPolicy(ep, vis, policy, ingressPolicyEnforced, egressPolicyEnforced, wg)
}

func (p *Proxy) RemoveNetworkPolicy(ep endpoint.EndpointInfoSource) {
	p.envoyIntegration.RemoveNetworkPolicy(ep)
}

// ChangeLogLevel changes proxy log level to correspond to the logrus log level 'level'.
func (p *Proxy) ChangeLogLevel(level logrus.Level) {
	if err := p.envoyIntegration.changeLogLevel(level); err != nil {
		log.WithError(err).Debug("failed to change log level in Envoy proxy")
	}

	if err := p.dnsIntegration.changeLogLevel(level); err != nil {
		log.WithError(err).Debug("failed to change log level in DNS proxy")
	}
}

// GetStatusModel returns the proxy status as API model
func (p *Proxy) GetStatusModel() *models.ProxyStatus {
	p.mutex.RLock()
	defer p.mutex.RUnlock()

	rangeMin, rangeMax, nPorts := p.proxyPorts.GetStatusInfo()

	result := &models.ProxyStatus{
		IP:             node.GetInternalIPv4Router().String(),
		PortRange:      fmt.Sprintf("%d-%d", rangeMin, rangeMax),
		TotalPorts:     int64(nPorts),
		TotalRedirects: int64(len(p.redirects)),
	}

	for name, redirect := range p.redirects {
		result.Redirects = append(result.Redirects, &models.ProxyRedirect{
			Name:      name,
			Proxy:     redirect.name,
			ProxyPort: int64(p.proxyPorts.GetRulesPort(redirect.listener)),
		})
	}
	result.EnvoyDeploymentMode = "embedded"
	if option.Config.ExternalEnvoyProxy {
		result.EnvoyDeploymentMode = "external"
	}
	return result
}

// updateRedirectMetrics updates the redirect metrics per application protocol
// in Prometheus. Lock needs to be held to call this function.
func (p *Proxy) updateRedirectMetrics() {
	result := map[string]int{}
	for _, redirect := range p.redirects {
		result[string(redirect.listener.ProxyType)]++
	}
	for proto, count := range result {
		metrics.ProxyRedirects.WithLabelValues(proto).Set(float64(count))
	}
}
