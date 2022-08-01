package modgearman

import (
	"fmt"
	"runtime/debug"
	"time"

	"github.com/appscode/g2/client"
	libworker "github.com/appscode/g2/worker"
)

type worker struct {
	id         string
	what       string
	worker     *libworker.Worker
	activeJobs int
	config     *configurationStruct
	mainWorker *mainWorker
	client     *client.Client
	dupclient  *client.Client
}

// creates a new worker and returns a pointer to it
func newWorker(what string, configuration *configurationStruct, mainWorker *mainWorker) *worker {
	logger.Tracef("starting new %sworker", what)
	worker := &worker{
		what:       what,
		activeJobs: 0,
		config:     configuration,
		mainWorker: mainWorker,
		client:     nil,
		dupclient:  nil,
	}
	worker.id = fmt.Sprintf("%p", worker)

	w := libworker.New(libworker.OneByOne)
	worker.worker = w

	w.ErrorHandler = func(e error) {
		worker.errorHandler(e)
	}

	worker.registerFunctions(configuration)

	// listen to this servers
	servers := mainWorker.ActiveServerList()
	if len(servers) == 0 {
		return nil
	}
	for _, address := range servers {
		status := worker.mainWorker.GetServerStatus(address)
		if status != "" {
			continue
		}
		err := w.AddServer("tcp", address)
		if err != nil {
			worker.mainWorker.SetServerStatus(address, err.Error())
			return nil
		}
	}

	// check if worker is ready
	if err := w.Ready(); err != nil {
		logger.Debugf("worker not ready closing again: %s", err.Error())
		worker.Shutdown()
		return nil
	}

	// start the worker
	go func() {
		defer logPanicExit()
		w.Work()
	}()

	return worker
}

func (worker *worker) registerFunctions(configuration *configurationStruct) {
	w := worker.worker
	// specifies what events the worker listens
	switch worker.what {
	case "check":
		if worker.config.eventhandler {
			w.AddFunc("eventhandler", worker.doWork, libworker.Unlimited)
		}
		if worker.config.hosts {
			w.AddFunc("host", worker.doWork, libworker.Unlimited)
		}
		if worker.config.services {
			w.AddFunc("service", worker.doWork, libworker.Unlimited)
		}
		if worker.config.notifications {
			w.AddFunc("notification", worker.doWork, libworker.Unlimited)
		}

		// register for the hostgroups
		if len(worker.config.hostgroups) > 0 {
			for _, element := range worker.config.hostgroups {
				w.AddFunc("hostgroup_"+element, worker.doWork, libworker.Unlimited)
			}
		}

		// register for servicegroups
		if len(worker.config.servicegroups) > 0 {
			for _, element := range worker.config.servicegroups {
				w.AddFunc("servicegroup_"+element, worker.doWork, libworker.Unlimited)
			}
		}
	case "status":
		statusQueue := fmt.Sprintf("worker_%s", configuration.identifier)
		w.AddFunc(statusQueue, worker.returnStatus, libworker.Unlimited)
	default:
		logger.Panicf("type not implemented: %s", worker.what)
	}
}

func (worker *worker) doWork(job libworker.Job) (res []byte, err error) {
	res = []byte("OK")
	logger.Tracef("worker got a job: %s", job.Handle())

	worker.activeJobs++

	received, err := decrypt((decodeBase64(string(job.Data()))), worker.config.encryption)
	if err != nil {
		logger.Errorf("decrypt failed: %s", err.Error())
		worker.activeJobs--
		return
	}
	taskCounter.WithLabelValues(received.typ).Inc()
	worker.mainWorker.tasks++

	logger.Debugf("incoming %s job: handle: %s - host: %s - service: %s", received.typ, job.Handle(), received.hostName, received.serviceDescription)
	logger.Trace(received)

	if worker.considerBalooning() {
		finChan := make(chan bool, 1)
		go func() {
			worker.executeJob(received)
			worker.activeJobs--
			if received.ballooning {
				worker.mainWorker.curBalooningWorker--
				balooningWorkerCount.Set(float64(worker.mainWorker.curBalooningWorker))
			}
			finChan <- true
		}()

		timeout := time.NewTimer(time.Duration(worker.config.backgroundingThreshold) * time.Second)
		ready := false
		for !ready {
			select {
			case <-finChan:
				logger.Debugf("job: %s finished", job.Handle())
				timeout.Stop()
				ready = true
			case <-timeout.C:
				// check again if are there open files left for balooning
				if !worker.startBalooning() {
					// wait another x seconds, all ballooning worker busy
					timeout = time.NewTimer(time.Duration(worker.config.backgroundingThreshold) * time.Second)
					continue
				}
				logger.Debugf("job: %s runs for more than %d seconds, backgrounding...", job.Handle(), worker.config.backgroundingThreshold)
				worker.mainWorker.curBalooningWorker++
				balooningWorkerCount.Set(float64(worker.mainWorker.curBalooningWorker))
				received.ballooning = true
				ready = true
			}
		}
	} else {
		worker.executeJob(received)
		worker.activeJobs--
	}

	return
}

// considerBalooning returns true if balooning is enabled and threshold is reached
func (worker *worker) considerBalooning() bool {
	if worker.config.backgroundingThreshold <= 0 {
		return false
	}

	// only if 70% of our workers are utilized
	if worker.mainWorker.workerUtilization < BalooningUtilizationThreshold {
		return false
	}

	return true
}

// startBalooning returns true if conditions for balooning are met (backgrounding jobs after backgroundingThreshold of seconds)
func (worker *worker) startBalooning() bool {
	if worker.config.backgroundingThreshold <= 0 {
		return false
	}

	if !worker.mainWorker.checkLoads() {
		return false
	}

	if !worker.mainWorker.checkMemory() {
		return false
	}

	// only if 70% of our workers are utilized
	if worker.mainWorker.workerUtilization < BalooningUtilizationThreshold {
		return false
	}

	// are there open files left for balooning
	if worker.mainWorker.curBalooningWorker >= (worker.mainWorker.maxPossibleWorker - worker.config.maxWorker) {
		return false
	}

	logger.Debugf("balooning: cur: %d max: %d", worker.mainWorker.curBalooningWorker, (worker.mainWorker.maxPossibleWorker - worker.config.maxWorker))
	return true
}

// executeJob executes the job and handles sending the result
func (worker *worker) executeJob(received *receivedStruct) {
	result := readAndExecute(received, worker.config)

	if result.returnCode > 0 {
		errorCounter.WithLabelValues(received.typ).Inc()
	}

	if received.resultQueue != "" {
		logger.Tracef("result:\n%s", result)
		enqueueServerResult(result)
		enqueueDupServerResult(worker.config, result)
	}
}

// errorHandler gets called if the libworker worker throws an error
func (worker *worker) errorHandler(e error) {
	switch err := e.(type) {
	case *libworker.WorkerDisconnectError:
		_, addr := err.Server()
		logger.Debugf("worker disconnect: %s from %s", e.Error(), addr)
		worker.mainWorker.SetServerStatus(addr, err.Error())
	default:
		logger.Errorf("worker error: %s", e.Error())
		logger.Errorf("%s", debug.Stack())
	}
	worker.Shutdown()
}

// Shutdown and deregister this worker
func (worker *worker) Shutdown() {
	logger.Debugf("worker shutting down")
	defer func() {
		if worker.mainWorker != nil && worker.mainWorker.running {
			worker.mainWorker.unregisterWorker(worker)
		}
	}()
	if worker.worker != nil {
		worker.worker.ErrorHandler = nil
		if worker.activeJobs == 0 {
			// try to stop gracefully
			worker.worker.Shutdown()
		}
		if worker.worker != nil {
			worker.worker.Close()
		}
	}
	if worker.client != nil {
		worker.client.Close()
		worker.client = nil
	}
	if worker.dupclient != nil {
		worker.dupclient.Close()
		worker.dupclient = nil
	}
	worker.worker = nil
}
