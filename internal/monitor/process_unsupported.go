//go:build !darwin && !linux && !windows

package monitor

func readProcessMetrics(int) (processMetrics, error) {
	return processMetrics{}, nil
}

func processChildRSS(int) (uint64, bool) {
	return 0, false
}
