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

package config

import (
	"github.com/bocheninc/L0/vm"
)

//VMConfig returns vm configuration
func VMConfig(logFile, logLevel string) *vm.Config {
	var config = vm.DefaultConfig()
	config.LogFile = logFile
	config.LogLevel = logLevel
	config.VMRegistrySize = getInt("vm.registrySize", config.VMRegistrySize)
	config.VMCallStackSize = getInt("vm.callStackSize", config.VMCallStackSize)
	config.VMMaxMem = getInt("vm.maxMem", config.VMMaxMem)
	config.ExecLimitStackDepth = getInt("vm.execLimitStackDepth", config.ExecLimitStackDepth)
	config.ExecLimitMaxOpcodeCount = getInt("vm.execLimitMaxOpcodeCount", config.ExecLimitMaxOpcodeCount)
	config.ExecLimitMaxRunTime = getInt("vm.execLimitMaxRunTime", config.ExecLimitMaxRunTime)
	config.ExecLimitMaxScriptSize = getInt("vm.execLimitMaxScriptSize", config.ExecLimitMaxScriptSize)
	config.ExecLimitMaxStateValueSize = getInt("vm.execLimitMaxStateValueSize", config.ExecLimitMaxStateValueSize)
	config.ExecLimitMaxStateItemCount = getInt("vm.execLimitMaxStateItemCount", config.ExecLimitMaxStateItemCount)
	config.ExecLimitMaxStateKeyLength = getInt("vm.execLimitMaxStateKeyLength", config.ExecLimitMaxStateKeyLength)
	config.LuaVMExeFilePath = getString("vm.luaVMExeFilePath", config.LuaVMExeFilePath)
	config.JSVMExeFilePath = getString("vm.jsVMExeFilePath", config.JSVMExeFilePath)
	config.BsWorkerCnt = getInt("vm.BsWorkerCnt", config.BsWorkerCnt)
	return config
}
