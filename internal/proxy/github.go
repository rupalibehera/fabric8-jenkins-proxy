package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io/ioutil"
	"net/http"
	"strings"
	"time"

	"github.com/fabric8-services/fabric8-jenkins-proxy/internal/clients"
	"github.com/fabric8-services/fabric8-jenkins-proxy/internal/storage"
	log "github.com/sirupsen/logrus"
)

const (
	// GHHeader is how we determine whether we are dealing with a GitHub webhook
	GHHeader = "User-Agent"
	// GHAgent is the prefix of the GHHeader
	GHAgent = "GitHub-Hookshot"
)

//GHHookStruct a simplified structure to get info from
//a webhook request
type GHHookStruct struct {
	Repository struct {
		Name     string `json:"name"`
		FullName string `json:"full_name"`
		GitURL   string `json:"git_url"`
		CloneURL string `json:"clone_url"`
	} `json:"repository"`
}

func (p *Proxy) handleGitHubRequest(w http.ResponseWriter, r *http.Request, requestLogEntry *log.Entry) (ns string, okToForward bool) {
	okToForward = false
	//Load request body if it's GH webhook
	gh := GHHookStruct{}
	defer r.Body.Close()

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}

	err = json.Unmarshal(body, &gh)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}

	requestLogEntry.WithField("json", gh).Debug("Processing GitHub JSON payload")

	namespace, err := p.getUserWithRetry(gh.Repository.CloneURL, requestLogEntry, defaultRetry)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}
	ns = namespace.Name
	clusterURL := namespace.ClusterURL
	nsLogger := requestLogEntry.WithField("ns", ns)
	nsLogger.WithFields(log.Fields{"cluster": clusterURL, "repository": gh.Repository.CloneURL}).Info("Processing GitHub request ")

	route, scheme, err := p.constructRoute(namespace.ClusterURL, namespace.Name)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}

	r.URL.Scheme = scheme
	r.URL.Host = route
	r.Host = route

	state, err := p.idler.State(ns, clusterURL)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}

	//If Jenkins is idle/stating, we need to cache the request and return success
	if state != clients.Running {
		p.storeGHRequest(w, r, ns, body, requestLogEntry)
		_, err = p.idler.UnIdle(ns, clusterURL)
		if err != nil {
			p.HandleError(w, err, requestLogEntry)
		}
		return
	}

	okToForward = true
	r.Body = ioutil.NopCloser(bytes.NewReader(body))
	//If Jenkins is up, we can simply proxy through
	nsLogger.Infof("Passing through %s", r.URL.String())
	return
}

func (p *Proxy) storeGHRequest(w http.ResponseWriter, r *http.Request, ns string, body []byte, requestLogEntry *log.Entry) {
	w.Header().Set("Server", "Webhook-Proxy")
	sr, err := storage.NewRequest(r, ns, body)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}
	err = p.storageService.CreateRequest(sr)
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}
	err = p.recordStatistics(ns, 0, time.Now().Unix())
	if err != nil {
		p.HandleError(w, err, requestLogEntry)
		return
	}

	requestLogEntry.WithField("ns", ns).Info("Webhook request buffered")
	w.WriteHeader(http.StatusAccepted)
	w.Write([]byte(""))
	return
}

//ProcessBuffer is a loop running through buffered webhook requests trying to replay them
func (p *Proxy) ProcessBuffer() {
	for {
		namespaces, err := p.storageService.GetUsers()
		if err != nil {
			log.Error(err)
		} else {
			for _, ns := range namespaces {
				requests, err := p.storageService.GetRequests(ns)
				if err != nil {
					log.Error(err)
					continue
				}
				for _, r := range requests {
					gh := GHHookStruct{}
					err = json.Unmarshal(r.Payload, &gh)
					if err != nil {
						log.Error(err)
						break
					}

					nsLogger := log.WithField("ns", ns)

					nsLogger.WithFields(log.Fields{"repository": gh.Repository.CloneURL}).Info("Retrying request")
					namespace, err := p.getUserWithRetry(gh.Repository.CloneURL, proxyLogger, defaultRetry)
					clusterURL := namespace.ClusterURL

					state, err := p.idler.State(ns, clusterURL)
					if err != nil {
						log.Error(err)
						break
					}
					err = p.recordStatistics(ns, 0, time.Now().Unix())
					if err != nil {
						log.Error(err)
					}
					if state == clients.Running {
						req, err := r.GetHTTPRequest()
						if err != nil {
							log.Errorf("Could not format request %s (%s): %s - deleting", r.ID, r.Namespace, err)
							err = p.storageService.DeleteRequest(&r)
							if err != nil {
								log.Errorf(storage.ErrorFailedDelete, r.ID, r.Namespace, err)
							}
							break
						}

						client := http.DefaultClient
						if r.Retries < p.maxRequestRetry { //Check how many times we retried since the Jenkins started
							resp, err := client.Do(req)
							if err != nil {
								log.Error("Error: ", err)
								errs := p.storageService.IncrementRequestRetry(&r)
								if len(errs) > 0 {
									for _, e := range errs {
										log.Error(e)
									}
								}
								break
							}

							if resp.StatusCode == 200 {
								nsLogger.Infof("Request to %q forwarded.", req.Host)
							} else if resp.StatusCode == 404 || resp.StatusCode == 400 {
								log.Warnf("Got status %q after retrying request on %s, throwing away the request", resp.Status, req.URL.String())
							} else {
								//Retry later if the response is not 200 or 400 or 404
								log.Errorf("Got status %q after retrying request on %s", resp.Status, req.URL.String())
								errs := p.storageService.IncrementRequestRetry(&r)
								for _, e := range errs {
									log.Error(e)
								}

								break
							}

						}

						// Deleting request since we tried too many times or the replay was successful with 200
						// or request was failed with 404 or 400
						err = p.storageService.DeleteRequest(&r)
						if err != nil {
							log.Errorf(storage.ErrorFailedDelete, r.ID, r.Namespace, err)
						}
					} else {
						//Do not try other requests for user if Jenkins is not running
						break
					}
				}
			}
		}
		time.Sleep(p.bufferCheckSleep)
	}
}

func (p *Proxy) getUserWithRetry(repositoryCloneURL string, logEntry *log.Entry, retry int) (clients.Namespace, error) {

	for i := 1; i < retry; i++ {
		if ns, err := p.getUser(repositoryCloneURL, logEntry); err == nil {
			return ns, nil
		}
		time.Sleep(1 * time.Second)
	}
	return p.getUser(repositoryCloneURL, logEntry)
}

//GetUser returns a namespace name based on GitHub repository URL
func (p *Proxy) getUser(repositoryCloneURL string, logEntry *log.Entry) (clients.Namespace, error) {
	if n, found := p.TenantCache.Get(repositoryCloneURL); found {
		namespace := n.(clients.Namespace)
		logEntry.WithFields(
			log.Fields{
				"ns": namespace,
			}).Infof("Cache hit for repository %s", repositoryCloneURL)
		return namespace, nil
	}

	logEntry.Infof("Cache miss for repository %s", repositoryCloneURL)
	wi, err := p.wit.SearchCodebase(repositoryCloneURL)
	if err != nil {
		return clients.Namespace{}, err
	}

	if len(strings.TrimSpace(wi.OwnedBy)) == 0 {
		return clients.Namespace{}, fmt.Errorf("unable to determine tenant id for repository %s", repositoryCloneURL)
	}

	logEntry.Infof("Found id %s for repo %s", wi.OwnedBy, repositoryCloneURL)
	ti, err := p.tenant.GetTenantInfo(wi.OwnedBy)
	if err != nil {
		return clients.Namespace{}, err
	}

	n, err := clients.GetNamespaceByType(ti, ServiceName)
	if err != nil {
		return clients.Namespace{}, err
	}

	p.TenantCache.SetDefault(repositoryCloneURL, n)
	return n, nil
}
