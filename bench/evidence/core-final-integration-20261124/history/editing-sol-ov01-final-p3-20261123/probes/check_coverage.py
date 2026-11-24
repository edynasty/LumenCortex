import json
import sys
from pathlib import Path

# Check actual test actions, not log text or wiring. Broad skips in 9ad5f2aa fail this check.
events = [json.loads(line) for line in Path(sys.argv[1]).read_text().splitlines() if line.startswith('{')]
status = {e['Test']: e['Action'] for e in events if e.get('Test') and e['Action'] in ('pass', 'fail', 'skip')}
required = (
    'TestWriteLeaseRefusesOtherHolder',
    'TestReviewEvictedReadRequiresFreshRevision',
    'TestReviewLeaseAcquisitionWaitsForCommit/create',
    'TestR201TwoOSProcessesCreateRace/concurrent_two_syscall_allow_create_stress/excl_after_signal/after_prepare',
    'TestR201TwoOSProcessesCreateRace/concurrent_two_syscall_allow_create_stress/excl_after_signal/before_publish',
    'TestR201TwoOSProcessesCreateRace/concurrent_atomic_allow_create_stress/after_prepare',
    'TestR201TwoOSProcessesCreateRace/concurrent_atomic_allow_create_stress/before_publish',
)
for test in required:
    print(f'{test}: {status.get(test, "NOT_EXECUTED")}')
sys.exit(0 if all(status.get(test) == 'pass' for test in required) else 1)
