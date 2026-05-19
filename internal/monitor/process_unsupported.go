//go:build !darwin && !linux && !windows

package monitor

func readProcessMetrics(int) (processMetrics, error) {
	return processMetrics{}, nil
}
