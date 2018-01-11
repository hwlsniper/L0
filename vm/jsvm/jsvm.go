// Copyright (C) 2017, Beijing Bochen Technology Co.,Ltd.  All rights reserved.
//
// This file is part of L0
//
// The L0 is free software: you can redistribute it and/or modify
// it under the terms of the GNU General Public License as published by
// the Free Software Foundation, either version 3 of the License, or
// (at your option) any later version.
//
// The L0 is distributed in the hope that it will be useful,
// but WITHOUT ANY WARRANTY; without even the implied warranty of
// MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
//
// GNU General Public License for more details.
//
// You should have received a copy of the GNU General Public License
// along with this program.  If not, see <http://www.gnu.org/licenses/>.

package jsvm

import (
	"errors"
	"fmt"
	"time"

	"github.com/bocheninc/L0/components/log"
	"github.com/robertkrimen/otto"
	"github.com/bocheninc/L0/vm"
	"strconv"
	"github.com/bocheninc/L0/core/params"
	"encoding/json"
	"github.com/bocheninc/L0/core/ledger/state"
	"github.com/bocheninc/L0/core/types"
)

type JsWorker struct {
	isInit bool
	isCanRedo bool
	VMConf *vm.Config
	workerProc *vm.WorkerProc
	ottoVM *otto.Otto
}

func NewJsWorker(conf *vm.Config) *JsWorker {
	worker := &JsWorker{isInit: false}
	worker.workerInit(true, conf)

	return worker
}

// VmJob handler main work
func (worker *JsWorker) VmJob(data interface{}) interface{} {
	worker.isCanRedo = false
	return worker.ExecJob(data)
}

// Exec worker
func (worker *JsWorker) ExecJob(data interface{}) interface{} {
	workerProcWithCallback := data.(*vm.WorkerProcWithCallback)
	result, err := worker.requestHandle(workerProcWithCallback.WorkProc)
	if err != nil {
		log.Errorf("execjob fail, result: %+v, err_msg: %+v", result, err.Error())
	}

	err = workerProcWithCallback.WorkProc.L0Handler.CallBack(&state.CallBackResponse{
		IsCanRedo: !worker.isCanRedo,
		Err: err,
		Result: result,
	})

	if err != nil && !worker.isCanRedo {
		worker.isCanRedo = true
		worker.ExecJob(data)
	}

	return result
}

// Block until worker ready
func (worker *JsWorker) VmReady() bool {
	return true
}

// initialize worker when started
func (worker *JsWorker) VmInitialize() {
	if !worker.isInit {
		worker.workerInit(true, vm.DefaultConfig())
	}
}

// terminate and clean resource when terminated
func (worker *JsWorker) VmTerminate() {

}

// handler all request
func (worker *JsWorker)requestHandle(wp *vm.WorkerProc) (interface{}, error) {
	txType := wp.ContractData.Transaction.GetType()
	if txType == types.TypeJSContractInit {
		return worker.InitContract(wp)
	} else if txType == types.TypeContractInvoke {
		return worker.InvokeExecute(wp)
	} else if txType == types.TypeContractQuery {
		return worker.QueryContract(wp)
	}

	return nil, errors.New(fmt.Sprintf("luavm no method match transaction type: %d", txType))
}

// RealInitContract real call L0Init and commit all change
func (worker *JsWorker) InitContract(wp *vm.WorkerProc) (interface{}, error) {
	err := worker.txTransfer()
	if err != nil {
		return nil, err
	}

	worker.resetProc(wp)
	worker.StoreContractCode()
	ok, err := worker.execContract(wp.ContractData, "L0Init")
	if !ok.(bool) || err != nil {
		return ok, err
	}

	err = worker.workerProc.CCallCommit()
	if err != nil {
		log.Errorf("commit all change error contractAddr:%s, errmsg:%s\n", worker.workerProc.ContractData.ContractAddr, err.Error())
		return false, err
	}

	return ok, err
}

// RealExecute real call L0Invoke and commit all change
func (worker *JsWorker) InvokeExecute(wp *vm.WorkerProc) (interface{}, error) {
	err := worker.txTransfer()
	if err != nil {
		return nil, err
	}

	worker.resetProc(wp)
	if len(wp.ContractData.ContractCode) == 0 {
		code, err := worker.GetContractCode()
		if err != nil {
			return nil, errors.New("can't get contract code")
		}
		wp.ContractData.ContractCode = string(code)
	}


	ok, err := worker.execContract(wp.ContractData, "L0Invoke")
	if !ok.(bool) || err != nil {
		return ok, err
	}

	err = worker.workerProc.CCallCommit()
	if err != nil {
		log.Errorf("commit all change error contractAddr:%s, errmsg:%s\n", worker.workerProc.ContractData.ContractAddr, err.Error())
		return false, err
	}

	return ok, err
}

// QueryContract call L0Query not commit change
func (worker *JsWorker)QueryContract(wp *vm.WorkerProc) ([]byte, error) {
	worker.resetProc(wp)
	result, err := worker.execContract(wp.ContractData, "L0Query")
	if err != nil {
		return nil, err
	}
	return []byte(result.(string)), nil
}

func (worker *JsWorker) resetProc(wp *vm.WorkerProc) {
	worker.workerProc = wp
	exporter(worker.ottoVM, worker.workerProc)
	wp.StateChangeQueue = vm.NewStateQueue()
	wp.TransferQueue = vm.NewTransferQueue()
}

func (worker *JsWorker) txTransfer() error {
	err := worker.workerProc.L0Handler.Transfer(worker.workerProc.ContractData.Transaction)
	if err != nil {
		return errors.New(fmt.Sprintf("Transfer failed..., err_msg: %s", err))
	}

	return nil
}

func (worker *JsWorker)workerInit(isInit bool, vmconf *vm.Config) {
	worker.VMConf = vmconf
	worker.workerProc = &vm.WorkerProc{}
	worker.ottoVM = otto.New()
	worker.ottoVM.SetOPCodeLimit(worker.VMConf.ExecLimitMaxOpcodeCount)
	worker.ottoVM.SetStackDepthLimit(worker.VMConf.ExecLimitStackDepth)
	worker.ottoVM.Interrupt = make(chan func(), 1) // The buffer prevents blocking
	worker.isInit = true
}


// execContract start a js vm and execute smart contract script
func (worker *JsWorker)execContract(cd *vm.ContractData, funcName string) (result interface{}, err error) {
	defer func() {
		if e := recover(); e != nil {
			result = false
			err = fmt.Errorf("exec contract code error: %v", e)
		}
	}()

	var val otto.Value
	if err = worker.CheckContractCode(cd.ContractCode); err != nil {
		return false, err
	}

	timeOut := time.Duration(worker.VMConf.ExecLimitMaxRunTime) * time.Millisecond
	timeOutChann := make(chan bool, 1)
	defer func() {
		timeOutChann <- true
	}()
	go func() {
		select {
		case <-timeOutChann:
		case <-time.After(timeOut):
			worker.ottoVM.Interrupt <- func() {
				panic(fmt.Errorf("code run: %v,time out", timeOut))
			}
		}
	}()

	_, err = worker.ottoVM.Run(cd.ContractCode)
	if err != nil {
		return false, err
	}

	val, err = callJSFunc(worker.ottoVM, cd, funcName)
	if err != nil {
		return false, err
	}

	if val.IsBoolean() {
		return val.ToBoolean()
	}
	return val.ToString()
}

func (worker *JsWorker) GetContractCode() (string, error) {
	var err error
	cc := new(vm.ContractCode)
	var code []byte
	if len(worker.workerProc.ContractData.ContractAddr) == 0 {
		code, err = worker.workerProc.L0Handler.GetGlobalState(params.GlobalContractKey)
	} else {
		code, err = worker.workerProc.L0Handler.GetState(vm.ContractCodeKey)
	}

	if len(code) != 0 && err == nil {
		contractCode, err := vm.DoContractStateData(code)
		if err != nil {
			return "", fmt.Errorf("cat't find contract code in db, err: %+v", err)
		}
		err = json.Unmarshal(contractCode, cc)
		if err != nil {
			return "", fmt.Errorf("cat't find contract code in db, err: %+v", err)
		}

		return string(cc.Code), nil
	} else {
		return "", errors.New("cat't find contract code in db")
	}
}

func (worker *JsWorker) StoreContractCode() error {
	code, err := vm.ConcrateStateJson(&vm.ContractCode{Code: []byte(worker.workerProc.ContractData.ContractCode), Type: "jsvm"})
	if err != nil {
		log.Errorf("Can't concrate contract code")
	}

	if len(worker.workerProc.ContractData.ContractAddr) == 0 {
		err = worker.workerProc.CCallPutState(params.GlobalContractKey, code.Bytes())
	} else {
		err = worker.workerProc.CCallPutState(vm.ContractCodeKey, code.Bytes()) // add js contract code into state
	}

	return  err
}

func (worker *JsWorker)CheckContractCode(code string) error {
	if len(code) == 0 || len(code) > worker.VMConf.ExecLimitMaxScriptSize {
		return errors.New("contract script code size illegal " +
			strconv.Itoa(len(code)) +
			"byte , max size is:" +
			strconv.Itoa(worker.VMConf.ExecLimitMaxScriptSize) + " byte")
	}

	return nil
}

func callJSFunc(ottoVM *otto.Otto, cd *vm.ContractData, funcName string) (val otto.Value, err error) {
	count := len(cd.ContractParams)
	if "L0Invoke" == funcName {
		if count == 0 {
			val, err = ottoVM.Call(funcName, nil, otto.NullValue(), otto.NullValue())
		} else if count == 1 {
			val, err = ottoVM.Call(funcName, nil, cd.ContractParams[0], otto.NullValue())
		} else {
			val, err = ottoVM.Call(funcName, nil, cd.ContractParams[0], cd.ContractParams[1:])
		}
	} else {
		if count == 0 {
			val, err = ottoVM.Call(funcName, nil, otto.NullValue())
		} else {
			val, err = ottoVM.Call(funcName, nil, cd.ContractParams)
		}
	}
	return
}