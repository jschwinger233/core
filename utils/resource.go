package utils

import resourcetypes "github.com/projecteru2/core/resources/types"

// GetCapacity .
func GetCapacity(scheduleInfos []resourcetypes.ScheduleInfo) map[string]int {
	capacity := make(map[string]int)
	for _, scheduleInfo := range scheduleInfos {
		capacity[scheduleInfo.Name] = scheduleInfo.Capacity
	}
	return capacity
}
