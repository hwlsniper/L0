package vm

import (
	"sync"
	"reflect"
	"sync/atomic"
	"errors"
	"time"
	"github.com/bocheninc/L0/components/log"
)

// start vm(lua and js service according to configure)
// manage the all worker instance


var (
	ErrVMAlreadyRunning = errors.New("vm have been running ...")
	ErrVMNotRunning = errors.New("vm not running")
	ErrJobNotFunc = errors.New("job not function")
	ErrWorkerClosed = errors.New("worker closed")
	ErrWorkerTimeout = errors.New("worker timeout")
)

type VirtualMachine struct {
	name string
	running uint32
	pendingAsyncJobs int32
	statusMutex sync.RWMutex
	selects   []reflect.SelectCase
	workers []*workerWrapper
	jobcnt  int
}

func (vm *VirtualMachine) isRunning() bool {
	return (atomic.LoadUint32(&vm.running) == 1)
}

func (vm *VirtualMachine) setRunning(running bool)  {
	if running {
		atomic.SwapUint32(&vm.running, 1)
	} else {
		atomic.SwapUint32(&vm.running, 0)
	}
}

func (vm *VirtualMachine) Loop() {
	for {
		select {

		}
	}
}

func (vm *VirtualMachine) Open(name string) (*VirtualMachine, error) {
	vm.statusMutex.Lock()
	defer vm.statusMutex.Unlock()

	if !vm.isRunning() {
		vm.selects = make([]reflect.SelectCase, len(vm.workers))

		for i, workerWrapper := range vm.workers {
			workerWrapper.Open()
			vm.selects[i] = reflect.SelectCase{
				Dir: reflect.SelectRecv,
				Chan: reflect.ValueOf(workerWrapper.readyChan),
			}
		}

		//go vm.Loop()
		vm.setRunning(true)
		return vm, nil
	}

	return nil, ErrVMAlreadyRunning
}

func (vm *VirtualMachine) Close(name string) error {
	vm.statusMutex.Lock()
	defer vm.statusMutex.Unlock()
	log.Debugf("vm to close, jobCnt: %d", vm.jobcnt)
	if vm.isRunning() {
		for _, workerWrapper := range vm.workers {
			workerWrapper.Close()
		}

		for _, workerWrapper := range vm.workers {
			workerWrapper.Join()
		}

		vm.setRunning(false)
		return nil
	}

	return ErrVMNotRunning
}

func CreateVM(numWorkers int, job func(interface{}) interface{}) *VirtualMachine {
	vm := &VirtualMachine{running: 0}

	vm.workers = make([]*workerWrapper, numWorkers)
	for i := range vm.workers {
		newWorker := workerWrapper{
			worker: &(VmDefaultWorker{&job}),
		}
		vm.workers[i] = &newWorker
	}
	//to manage workers
	NewTxSync(numWorkers)

	return vm
}

func CreateGenericVM(numWorkers int, fn func(interface{}) interface{}) *VirtualMachine {
	return CreateVM(numWorkers, fn)
}

func CreateCustomVM(workers []VmWorker) *VirtualMachine {

	vm := &VirtualMachine{running: 0}
	vm.workers = make([]*workerWrapper, len(workers))
	for i := range vm.workers {
		newWorker := workerWrapper{
			worker: workers[i],
		}
		vm.workers[i] = &newWorker
	}

	//to manage workers
	return vm
}


func (vm *VirtualMachine) SendWorkTimed(timeout time.Duration, jobData interface{}) (interface{}, error) {
	vm.statusMutex.Lock()
	defer vm.statusMutex.Unlock()

	if vm.isRunning() {
		before := time.Now()

		startTimeOut := time.NewTimer(timeout * time.Millisecond)
		defer startTimeOut.Stop()

		selectCases := append(vm.selects, reflect.SelectCase{
			Dir:reflect.SelectRecv,
			Chan:reflect.ValueOf(startTimeOut.C),
		})

		if chosen, _, ok := reflect.Select(selectCases); ok {
			if chosen < (len(selectCases) - 1) {
				vm.workers[chosen].jobChan <- jobData

				timeoutRemain := time.NewTimer(timeout * time.Millisecond - time.Since(before))
				defer timeoutRemain.Stop()

				select {
				case data, open := <-vm.workers[chosen].outputChan:
					if !open {
						return nil, ErrWorkerClosed
					}

					return data, nil
				case <- timeoutRemain.C:
					go func() {
						vm.workers[chosen].Interrupt()
						<-vm.workers[chosen].outputChan
					}()

					return nil, ErrWorkerTimeout
				}

			} else {
				return nil, ErrWorkerTimeout
			}

		} else {
			return nil, ErrWorkerClosed
		}
	} else {
		return nil, ErrVMNotRunning
	}
}

func (vm *VirtualMachine) SendWorkTimedAsync(timeout time.Duration, jobData interface{}, callback func(interface{}, error)) {
	atomic.AddInt32(&vm.pendingAsyncJobs, 1)

	go func() {
		defer atomic.AddInt32(&vm.pendingAsyncJobs, -1)
		result, err := vm.SendWorkTimed(timeout, jobData)
		if callback != nil {
			callback(result, err)
		}
	}()
}

func (vm *VirtualMachine) SendWork(jobData interface{}) (interface{}, error) {
	//vm.statusMutex.Lock()
	//defer vm.statusMutex.Unlock()

	if vm.isRunning() {
		if chose, _, ok := reflect.Select(vm.selects); ok && chose >= 0 {
			//log.Debugf("chose: %+v, same_cnt: %+v", chose, len(vm.selects))
			vm.workers[chose].jobChan <- jobData
			result, open := <-vm.workers[chose].outputChan

			if !open {
				return nil, ErrWorkerClosed
			}

			return result, nil
		}

		return nil, ErrWorkerClosed
	}

	return nil, ErrVMNotRunning
}

func (vm *VirtualMachine)SendWorkAsync(jobData interface{}, callback func(interface{}, error)) {
	atomic.AddInt32(&vm.pendingAsyncJobs, 1)

	go func() {
		defer atomic.AddInt32(&vm.pendingAsyncJobs, -1)
		result, err := vm.SendWork(jobData)
		if callback != nil {
			callback(result, err)
		}
	}()
}


func (vm *VirtualMachine) SendWorkClean(jobData interface{}) (interface{}, error) {
	//vm.statusMutex.Lock()
	//defer vm.statusMutex.Unlock()

	if vm.isRunning() {
		if chose, _, ok := reflect.Select(vm.selects); ok && chose >= 0 {
			//log.Debugf("chose: %+v, same_cnt: %+v", chose, len(vm.selects))
			vm.jobcnt ++
			vm.workers[chose].jobChan <- jobData
		}

		return nil, ErrWorkerClosed
	}

	return nil, ErrVMNotRunning
}


func (vm *VirtualMachine)SendWorkCleanAsync(jobData interface{}) error {
	atomic.AddInt32(&vm.pendingAsyncJobs, 1)
	defer atomic.AddInt32(&vm.pendingAsyncJobs, -1)

	_, err := vm.SendWorkClean(jobData)

	return err
}

func (vm *VirtualMachine) NumPendingAsycnJobs() int32 {
	return atomic.LoadInt32(&vm.pendingAsyncJobs)
}

func (vm *VirtualMachine) NumWorkers() int {
	return len(vm.workers)
}
