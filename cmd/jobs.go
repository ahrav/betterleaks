package cmd

import "runtime"

type jobProfile uint8

const (
	directoryJobProfile jobProfile = iota
	objectJobProfile
	streamJobProfile
	gitJobProfile
	providerJobProfile
)

const (
	maxAutomaticProviderJobs  = 4
	maxAutomaticGitJobs       = 4
	automaticFileJobsPerCPU   = 4
	automaticObjectJobsPerCPU = 2

	// maxAutomaticFileJobs caps concurrent file readers. Reads from the page
	// cache are cheap and the kernel serializes concurrent opens within a
	// process, so past ~32 readers more concurrency only adds open contention
	// and descriptor-table growth; 32 still hides cold-cache latency. On a
	// 64-core host the previous rule (one reader per CPU) measured 5-7%
	// lower throughput than 32 readers.
	maxAutomaticFileJobs = 32
)

type jobPlan struct {
	Source   int
	Detector int
}

func resolveJobPlan(configured int, profile jobProfile) jobPlan {
	processorJobs := max(runtime.GOMAXPROCS(0), 1)
	if configured > 0 {
		sourceJobs := configured
		if profile == gitJobProfile {
			sourceJobs = min(sourceJobs, processorJobs)
		}
		return jobPlan{
			Source:   sourceJobs,
			Detector: min(configured, processorJobs),
		}
	}

	switch profile {
	case directoryJobProfile:
		return jobPlan{
			Source:   min(processorJobs*automaticFileJobsPerCPU, maxAutomaticFileJobs),
			Detector: processorJobs,
		}
	case objectJobProfile:
		return jobPlan{
			Source:   processorJobs * automaticObjectJobsPerCPU,
			Detector: processorJobs,
		}
	case streamJobProfile:
		return jobPlan{Source: processorJobs, Detector: processorJobs}
	case gitJobProfile:
		return jobPlan{Source: min(processorJobs, maxAutomaticGitJobs), Detector: processorJobs}
	case providerJobProfile:
		jobs := min(processorJobs, maxAutomaticProviderJobs)
		return jobPlan{Source: jobs, Detector: jobs}
	default:
		panic("unknown job profile")
	}
}
