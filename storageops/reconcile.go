// Package storageops implements operations on the state directory.
package storageops

import (
	"context"
	"crypto"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"fmt"
	"git.devever.net/hlandau/acmeapi"
	"git.devever.net/hlandau/acmeapi/acmeendpoints"
	"github.com/hlandau/acmetool/hooks"
	"github.com/hlandau/acmetool/responder"
	"github.com/hlandau/acmetool/solver"
	"github.com/hlandau/acmetool/storage"
	"github.com/hlandau/acmetool/util"
	"github.com/hlandau/xlog"
	"github.com/jmhodges/clock"
	"net/http"
	"sort"
	"strings"
	"time"
)

var log, Log = xlog.New("acmetool.storageops")

// Internal use only. Used for testing purposes. Do not change.
var InternalClock = clock.Default()

// Internal use only. Used for testing purposes. Do not change.
var InternalHTTPClient *http.Client

type reconcile struct {
	store storage.Store

	// Cache of account clients to avoid duplicated directory lookups.
	accountClients map[*storage.Account]*acmeapi.RealmClient
}

func makeReconcile(store storage.Store) *reconcile {
	return &reconcile{
		store:          store,
		accountClients: map[*storage.Account]*acmeapi.RealmClient{},
	}
}

func EnsureRegistration(store storage.Store) error {
	return makeReconcile(store).EnsureRegistration()
}

func (r *reconcile) EnsureRegistration() error {
	a, err := r.getAccountByDirectoryURL("")
	if err != nil {
		return err
	}

	cl, err := r.getClientForAccount(a)
	if err != nil {
		return err
	}

	return solver.AssistedRegistration(context.TODO(), cl, a.ToAPI(), nil)
}

func Reconcile(store storage.Store) error {
	r := makeReconcile(store)

	reconcileErr := r.Reconcile()
	log.Errore(reconcileErr, "failed to reconcile")

	reloadErr := r.store.Reload()
	log.Errore(reloadErr, "failed to reload after reconciliation")

	relinkErr := r.Relink()
	log.Errore(relinkErr, "failed to relink after reconciliation")

	err := reconcileErr
	if err == nil {
		err = reloadErr
	}
	if err == nil {
		err = relinkErr
	}

	return err
}

func Relink(store storage.Store) error {
	err := makeReconcile(store).Relink()
	log.Errore(err, "failed to relink")
	return err
}

func (r *reconcile) Relink() error {
	hostnameTargetMapping, err := r.disjoinTargets()
	if err != nil {
		return err
	}

	var updatedHostnames []string

	for name, tgt := range hostnameTargetMapping {
		c, err := FindBestCertificateSatisfying(r.store, tgt)
		if err != nil {
			log.Debugf("could not find certificate satisfying %v: %v", tgt, err)
			continue
		}

		log.Tracef("relink: best certificate satisfying %v is %v", tgt, c)

		cprev, err := r.store.PreferredCertificateForHostname(name)

		if c != cprev || err != nil {
			log.Debugf("relinking: %v -> %v (was %v)", name, c, cprev)
			updatedHostnames = append(updatedHostnames, name)

			err = r.store.SetPreferredCertificateForHostname(name, c)
			log.Errore(err, "failed to set preferred certificate for hostname")
		}
	}

	ctx := &hooks.Context{
		HooksDir: "",
		StateDir: r.store.Path(),
	}

	err = hooks.NotifyLiveUpdated(ctx, updatedHostnames) // ignore error
	log.Errore(err, "failed to call notify hooks")

	return nil
}

func (r *reconcile) disjoinTargets() (hostnameTargetMapping map[string]*storage.Target, err error) {
	var targets []*storage.Target

	r.store.VisitTargets(func(t *storage.Target) error {
		targets = append(targets, t)
		return nil
	})

	sort.Stable(sort.Reverse(targetSorter(targets)))

	// Hostname-target mapping.
	hostnameTargetMapping = map[string]*storage.Target{}
	for _, tgt := range targets {
		tgt.Satisfy.ReducedNames = nil
		for _, name := range tgt.Satisfy.Names {
			_, exists := hostnameTargetMapping[name]
			if !exists {
				hostnameTargetMapping[name] = tgt
				tgt.Satisfy.ReducedNames = append(tgt.Satisfy.ReducedNames, name)
			}
		}
	}

	// Debugging information.
	for name, tgt := range hostnameTargetMapping {
		log.Debugf("disjoint hostname mapping: %s -> %v", name, tgt)
	}

	return
}

func (r *reconcile) Reconcile() error {
	err := r.processUncachedCertificates()
	if err != nil {
		return err
	}

	//err = r.processPendingRevocations()
	//log.Errore(err, "could not process pending revocations")

	err = r.processTargets()
	log.Errore(err, "error while processing targets")
	if err != nil {
		return err
	}

	return nil
}

func (r *reconcile) processUncachedCertificates() error {
	if !HaveUncachedCertificates(r.store) {
		return nil
	}

	log.Debug("there are uncached certificates - downloading them")

	err := r.downloadUncachedCertificates()
	if err != nil {
		log.Errore(err, "error while downloading uncached certificates")
		return err
	}

	log.Debug("reloading after downloading uncached certificates")
	err = r.store.Reload()
	if err != nil {
		log.Errore(err, "failed to reload after downloading uncached certificates")
		return err
	}

	log.Debug("finished reloading after downloading uncached certificates")

	if HaveUncachedCertificates(r.store) {
		log.Error("failed to download all uncached certificates")
		return fmt.Errorf("cannot obtain one or more uncached certificates")
	}

	return nil
}

func (r *reconcile) downloadUncachedCertificates() error {
	return r.store.VisitCertificates(func(c *storage.Certificate) error {
		if c.Cached {
			return nil
		}

		err := r.downloadCertificateAdaptive(c)
		if err != nil {
			// If the download fails, consider whether the error is permanent or
			// temporary. If temporary, don't hold up other certificates and continue
			// for now. We'll try again when next invoked.
			if util.IsTemporary(err) {
				// continue visitation
				log.Errore(err, "temporary error when trying to download certificate")
				return nil
			} else {
				// Permanent error, stop.
				// TODO: We might want to switch this to deleting the certificate at
				// some point.
				return err
			}
		}

		return nil
	})
}

func (r *reconcile) getAccountByDirectoryURL(directoryURL string) (*storage.Account, error) {
	if directoryURL == "" {
		directoryURL = r.store.DefaultTarget().Request.Provider
	}

	if directoryURL == "" {
		directoryURL = acmeendpoints.DefaultEndpoint.DirectoryURL
	}

	if !acmeapi.ValidURL(directoryURL) {
		return nil, fmt.Errorf("directory URL is not a valid HTTPS URL")
	}

	ma := r.store.AccountByDirectoryURL(directoryURL)
	if ma != nil {
		return ma, nil
	}

	return r.createNewAccount(directoryURL)
}

func (r *reconcile) createNewAccount(directoryURL string) (*storage.Account, error) {
	pk, err := generateKey(&r.store.DefaultTarget().Request.Key)
	if err != nil {
		return nil, err
	}

	a := &storage.Account{
		PrivateKey:   pk,
		DirectoryURL: directoryURL,
	}

	err = r.store.SaveAccount(a)
	if err != nil {
		log.Errore(err, "failed to save account")
		return nil, err
	}

	return a, nil
}

func (r *reconcile) getGenericClient() (*acmeapi.RealmClient, error) {
	return r.getClientForDirectoryURL("")
}

func (r *reconcile) getClientForDirectoryURL(directoryURL string) (*acmeapi.RealmClient, error) {
	return acmeapi.NewRealmClient(acmeapi.RealmClientConfig{
		DirectoryURL: directoryURL,
		HTTPClient:   InternalHTTPClient,
	})
}

func (r *reconcile) getClientForAccount(a *storage.Account) (*acmeapi.RealmClient, error) {
	cl := r.accountClients[a]
	if cl == nil {
		var err error
		cl, err = r.getClientForDirectoryURL(a.DirectoryURL)
		if err != nil {
			return nil, err
		}

		r.accountClients[a] = cl
	}

	return cl, nil
}

func (r *reconcile) processTargets() error {
	var merr util.MultiError

	r.store.VisitTargets(func(t *storage.Target) error {
		c, err := FindBestCertificateSatisfying(r.store, t)
		log.Debugf("%v: best certificate satisfying is %v, err=%v", t, c, err)
		if err == nil && !CertificateNeedsRenewing(c) {
			log.Debugf("%v: have best certificate which does not need renewing, skipping target", t)
			return nil // continue
		}

		log.Debugf("%v: requesting certificate", t)
		err = r.requestCertificateForTarget(t)
		log.Errore(err, t, ": failed to request certificate")
		if err != nil {
			// Do not block satisfaction of other targets just because one fails;
			// collect errors and return them as one.
			merr = append(merr, &TargetSpecificError{
				Target: t,
				Err:    err,
			})
		}

		return nil
	})

	log.Debugf("done processing targets, reconciliation complete, %d errors occurred", len(merr))

	if len(merr) != 0 {
		return merr
	}

	return nil
}

func (r *reconcile) getRequestAccount(tr *storage.TargetRequest) (*storage.Account, error) {
	if tr.Account != nil {
		return tr.Account, nil
	}

	// This will create the account if it doesn't exist.
	acct, err := r.getAccountByDirectoryURL(tr.Provider)
	if err != nil {
		return nil, err
	}

	return acct, nil
}

func (r *reconcile) requestCertificateForTarget(t *storage.Target) error {
	ensureConceivablySatisfiable(t)

	acct, err := r.getRequestAccount(&t.Request)
	if err != nil {
		return err
	}

	cl, err := r.getClientForAccount(acct)
	if err != nil {
		return err
	}

	apiAcct := acct.ToAPI()

	err = solver.AssistedRegistration(context.TODO(), cl, apiAcct, nil)
	if err != nil {
		return err
	}

	csr, err := r.createCSR(t)
	if err != nil {
		return err
	}

	orderTpl := acmeapi.Order{}
	for _, name := range t.Request.Names {
		orderTpl.Identifiers = append(orderTpl.Identifiers, acmeapi.Identifier{
			Type:  acmeapi.IdentifierTypeDNS,
			Value: name,
		})
	}

	log.Debugf("%v: ordering certificate", t)
	order, err := solver.Order(context.TODO(), cl, apiAcct, &orderTpl, csr, r.targetToChallengeConfig(t))
	if err != nil {
		return err
	}

	c, err := r.store.ImportCertificate(order.URL)
	if err != nil {
		log.Errore(err, "could not import certificate")
		return err
	}

	err = r.downloadCertificateAdaptive(c)
	if err != nil {
		return err
	}

	return nil
}

func (r *reconcile) targetToChallengeConfig(t *storage.Target) *responder.ChallengeConfig {
	trc := &t.Request.Challenge
	hctx := &hooks.Context{
		HooksDir: "",
		StateDir: r.store.Path(),
		Env:      map[string]string{},
	}
	for k, v := range trc.InheritedEnv {
		hctx.Env[k] = v
	}
	for k, v := range trc.Env {
		hctx.Env[k] = v
	}

	startHookFunc := func(challengeInfo interface{}) error {
		switch v := challengeInfo.(type) {
		case *responder.HTTPChallengeInfo:
			_, err := hooks.ChallengeHTTPStart(hctx, v.Hostname, t.Filename, v.Filename, v.Body)
			return err
		case *responder.DNSChallengeInfo:
			installed, err := hooks.ChallengeDNSStart(hctx, v.Hostname, t.Filename, v.Body)
			if err == nil && !installed {
				return fmt.Errorf("could not install DNS challenge, no hooks succeeded")
			}
			return err
		default:
			return nil
		}
	}

	stopHookFunc := func(challengeInfo interface{}) error {
		switch v := challengeInfo.(type) {
		case *responder.HTTPChallengeInfo:
			return hooks.ChallengeHTTPStop(hctx, v.Hostname, t.Filename, v.Filename, v.Body)
		case *responder.DNSChallengeInfo:
			uninstalled, err := hooks.ChallengeDNSStop(hctx, v.Hostname, t.Filename, v.Body)
			if err == nil && !uninstalled {
				return fmt.Errorf("could not uninstall DNS challenge, no hooks succeeded")
			}
			return err
		default:
			return nil
		}
	}

	httpSelfTest := true
	if trc.HTTPSelfTest != nil {
		httpSelfTest = *trc.HTTPSelfTest
	}

	return &responder.ChallengeConfig{
		WebPaths:       trc.WebrootPaths,
		HTTPPorts:      trc.HTTPPorts,
		HTTPNoSelfTest: !httpSelfTest,
		StartHookFunc:  startHookFunc,
		StopHookFunc:   stopHookFunc,
	}
}

var (
	oidTLSFeature          = asn1.ObjectIdentifier{1, 3, 6, 1, 5, 5, 7, 1, 24}
	mustStapleFeatureValue = []byte{0x30, 0x03, 0x02, 0x01, 0x05}
)

func (r *reconcile) createCSR(t *storage.Target) ([]byte, error) {
	if len(t.Request.Names) == 0 {
		return nil, fmt.Errorf("cannot request a certificate with no names")
	}

	csr := &x509.CertificateRequest{
		DNSNames: t.Request.Names,
		Subject: pkix.Name{
			CommonName: t.Request.Names[0],
		},
	}

	if t.Request.OCSPMustStaple {
		csr.ExtraExtensions = append(csr.ExtraExtensions, pkix.Extension{
			Id:    oidTLSFeature,
			Value: mustStapleFeatureValue,
		})
	}

	pk, err := r.generateOrGetKey(&t.Request.Key)
	if err != nil {
		log.Errore(err, "could not generate key while generating CSR for %v", t)
		return nil, err
	}

	_, err = r.store.ImportKey(pk)
	if err != nil {
		log.Errore(err, "could not import freshly generated key while generating CSR for %v", t)
		return nil, err
	}

	csr.SignatureAlgorithm, err = signatureAlgorithmFromKey(pk)
	if err != nil {
		return nil, err
	}

	return x509.CreateCertificateRequest(rand.Reader, csr, pk)
}

func (r *reconcile) generateOrGetKey(trk *storage.TargetRequestKey) (crypto.PrivateKey, error) {
	if trk.ID != "" {
		k := r.store.KeyByID(strings.TrimSpace(strings.ToLower(trk.ID)))
		if k != nil {
			return k.PrivateKey, nil
		}

		log.Warnf("target requests specific key %q but it cannot be found, generating a new key", trk.ID)
	}

	return generateKey(trk)
}

func (r *reconcile) downloadCertificateAdaptive(c *storage.Certificate) error {
	log.Debugf("downloading certificate %v", c)

	cl, err := r.getGenericClient()
	if err != nil {
		return err
	}

	order := &acmeapi.Order{}
	cert := &acmeapi.Certificate{}
	isCert, err := cl.LoadOrderOrCertificate(context.TODO(), c.URL, order, cert)
	if err != nil {
		return err
	}

	if !isCert {
		// It's an order URL, so we need to a) wait for the order to be complete,
		// and b) download the certificate via the URL given.

		// Wait for the order to be complete.
		waitLimit := time.Now().Add(10 * time.Minute)
		for !order.Status.IsFinal() {
			// How long should it take for an order to finish after finalization is
			// requested? Probably not long for the Let's Encrypt use case, but it's
			// not hard to imagine weird implementations where there's a long waiting
			// time (e.g. manual approval). We don't want to hang forever waiting, so
			// let's bail after a while in the expectation we'll try again later when
			// cron next invokes us.
			if time.Now().After(waitLimit) {
				err = fmt.Errorf("took more than 10 minutes to wait for an order (%q, status %q) to become final; giving up for now", order.URL, order.Status)
				err = util.NewPertError(true, err)
				return err
			}

			err = cl.WaitLoadOrder(context.TODO(), order)
			if err != nil {
				return err
			}
		}

		if order.Status != acmeapi.OrderValid {
			// Order is final not not valid, which means the server has reneged on an
			// order after finalisation. Not sure whether this can happen, but it
			// wouldn't surprise me if such implementations show up. As per ACME-SSS,
			// we should treat this as a 'permanent error' and delete the certificate.
			return fmt.Errorf("order became invalid after finalisation: %q (%q)", order.URL, order.Status)
		}

		// Download the certificate.
		cert = &acmeapi.Certificate{
			URL: order.CertificateURL,
		}

		err = cl.LoadCertificate(context.TODO(), cert)
		if err != nil {
			return err
		}
	}

	// At this point we have the certificate in 'cert', and cert.URL is set.
	if len(cert.CertificateChain) == 0 {
		return fmt.Errorf("nil certificate?")
	}

	c.Certificates = cert.CertificateChain
	c.Cached = true

	err = r.store.SaveCertificate(c)
	if err != nil {
		log.Errore(err, "failed to save certificate after retrieval: %v", c)
		return err
	}

	return nil
}

// todo change solver.Order to not wait for finalisation
