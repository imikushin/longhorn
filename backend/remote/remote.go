package remote

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/Sirupsen/logrus"
	"github.com/pkg/errors"
	"github.com/rancher/longhorn/replica/rest"
	"github.com/rancher/longhorn/rpc"
	"github.com/rancher/longhorn/types"
	"github.com/rancher/longhorn/util"
	journal "github.com/rancher/sparse-tools/stats"
)

var (
	pingRetries = 6
	pingTimeout = 20 * time.Second
	pingInveral = 2 * time.Second

	timeout        = 30 * time.Second
	requestBuffer  = 1024
	ErrPingTimeout = errors.New("Ping timeout")
)

func New() types.BackendFactory {
	return &Factory{}
}

type Factory struct {
}

type Remote struct {
	types.ReaderWriterAt
	name       string
	pingURL    string
	replicaURL string
	httpClient *http.Client
	closeChan  chan struct{}
}

func (r *Remote) Close() error {
	r.closeChan <- struct{}{}
	logrus.Infof("Closing: %s", r.name)
	return r.doAction("close", "")
}

func (r *Remote) open() error {
	logrus.Infof("Opening: %s", r.name)
	return r.doAction("open", "")
}

func (r *Remote) Snapshot(name string) error {
	logrus.Infof("Snapshot: %s %s", r.name, name)
	return r.doAction("snapshot", name)
}

func (r *Remote) doAction(action, name string) error {
	body := io.Reader(nil)
	if name != "" {
		buffer := &bytes.Buffer{}
		if err := json.NewEncoder(buffer).Encode(&map[string]string{"name": name}); err != nil {
			return err
		}
		body = buffer
	}

	req, err := http.NewRequest("POST", r.replicaURL+"?action="+action, body)
	if err != nil {
		return err
	}

	if name != "" {
		req.Header.Add("Content-Type", "application/json")
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bad status: %d %s", resp.StatusCode, resp.Status)
	}

	return nil
}

func (r *Remote) Size() (int64, error) {
	replica, err := r.info()
	if err != nil {
		return 0, err
	}
	return strconv.ParseInt(replica.Size, 10, 0)
}

func (r *Remote) SectorSize() (int64, error) {
	replica, err := r.info()
	if err != nil {
		return 0, err
	}
	return replica.SectorSize, nil
}

func (r *Remote) RemainSnapshots() (int, error) {
	replica, err := r.info()
	if err != nil {
		return 0, err
	}
	if replica.State != "open" && replica.State != "dirty" && replica.State != "rebuilding" {
		return 0, fmt.Errorf("Invalid state %v for counting snapshots", replica.State)
	}
	return replica.RemainSnapshots, nil
}

func (r *Remote) GetRevisionCounter() (int64, error) {
	replica, err := r.info()
	if err != nil {
		return 0, err
	}
	if replica.State != "open" && replica.State != "dirty" {
		return 0, fmt.Errorf("Invalid state %v for getting revision counter", replica.State)
	}
	return replica.RevisionCounter, nil
}

func (r *Remote) SetRevisionCounter(counter int64) error {
	body := &bytes.Buffer{}
	if err := json.NewEncoder(body).Encode(&map[string]int64{"counter": counter}); err != nil {
		return err
	}

	req, err := http.NewRequest("POST", r.replicaURL+"?action=setrevisioncounter", body)
	if err != nil {
		return err
	}
	req.Header.Add("Content-Type", "application/json")

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("Bad status: %d %s", resp.StatusCode, resp.Status)
	}

	return nil
}

func (r *Remote) info() (rest.Replica, error) {
	var replica rest.Replica
	req, err := http.NewRequest("GET", r.replicaURL, nil)
	if err != nil {
		return replica, err
	}

	resp, err := r.httpClient.Do(req)
	if err != nil {
		return replica, err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return replica, fmt.Errorf("Bad status: %d %s", resp.StatusCode, resp.Status)
	}

	err = json.NewDecoder(resp.Body).Decode(&replica)
	return replica, err
}

var httpClient = &http.Client{Timeout: 10 * time.Second}

func waitForOK(attempts int, url string, errCh chan<- error) {
	resp, err := httpClient.Get(url)
	if err != nil {
		logrus.Debugf("%v", errors.Wrapf(err, "error getting '%s'", url))
		<-time.NewTimer(time.Second).C
		go waitForOK(attempts-1, url, errCh)
		return
	}
	resp.Body.Close()

	if 200 <= resp.StatusCode && resp.StatusCode < 300 {
		logrus.Infof("Got OK from '%s'", url)
		errCh <- nil
		return
	}
	if attempts <= 0 {
		errCh <- errors.Errorf("giving up getting '%s'", url)
		return
	}

	<-time.NewTimer(time.Second).C
	go waitForOK(attempts-1, url, errCh)
}

func (rf *Factory) Wait(address string) error {
	controlAddress, _, _, err := util.ParseAddresses(address)
	if err != nil {
		return fmt.Errorf("wait: error parsing address '%s': %v", address, err)
	}
	url := "http://" + controlAddress + "/v1"
	errCh := make(chan error)
	defer close(errCh)
	go waitForOK(30, url, errCh)
	return <-errCh
}

func (rf *Factory) WaitAll(addresses ...string) error {
	return errors.New("not implemented: use dynamic factory for WaitAll")
}

func (rf *Factory) Create(address string) (types.Backend, error) {
	logrus.Infof("Connecting to remote: %s", address)

	controlAddress, dataAddress, _, err := util.ParseAddresses(address)
	if err != nil {
		return nil, err
	}

	r := &Remote{
		name:       address,
		replicaURL: fmt.Sprintf("http://%s/v1/replicas/1", controlAddress),
		pingURL:    fmt.Sprintf("http://%s/ping", controlAddress),
		httpClient: &http.Client{
			Timeout: timeout,
		},
		closeChan: make(chan struct{}, 1),
	}

	replica, err := r.info()
	if err != nil {
		return nil, err
	}

	if replica.State != "closed" {
		return nil, fmt.Errorf("Replica must be closed, Can not add in state: %s", replica.State)
	}

	conn, err := net.Dial("tcp", dataAddress)
	if err != nil {
		return nil, err
	}

	rpc := rpc.NewClient(conn)
	r.ReaderWriterAt = rpc

	if err := r.open(); err != nil {
		return nil, err
	}

	go r.monitorPing(rpc)

	return r, nil
}

func (r *Remote) monitorPing(client *rpc.Client) error {
	ticker := time.NewTicker(pingInveral)
	defer ticker.Stop()

	retry := 0
	for {
		select {
		case <-r.closeChan:
			return nil
		case <-ticker.C:
			if err := r.Ping(client); err == nil {
				retry = 0 // reset on success
			} else {
				if retry < pingRetries {
					retry++
					logrus.Errorf("Ping retry %v on replica %v; error: %v", retry, r.replicaURL, err)
					journal.PrintLimited(1000) //flush automatically upon retry
				} else {
					logrus.Errorf("Failed to get ping response from replica %v; error: %v", r.replicaURL, err)
					journal.PrintLimited(1000) //flush automatically upon error
					client.SetError(err)
					return err
				}
			}
		}
	}
}

func (r *Remote) Ping(client *rpc.Client) error {
	ret := make(chan error, 1)
	// use replica data addr for ping target tracking
	opID := journal.InsertPendingOp(time.Now(), client.TargetID(), journal.OpPing, 0)

	go func() {
		resp, err := r.httpClient.Get(r.pingURL)
		if err != nil {
			ret <- err
			return
		}
		defer resp.Body.Close()
		if resp.StatusCode != 200 {
			ret <- fmt.Errorf("Non-200 response %d from ping to %s", resp.StatusCode, r.name)
			return
		}
		ret <- nil
	}()

	select {
	case err := <-ret:
		journal.RemovePendingOp(opID, err == nil)
		return err
	case <-time.After(pingTimeout):
		journal.RemovePendingOp(opID, false)
		return ErrPingTimeout
	}
}
