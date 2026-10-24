#!/usr/bin/env python3
"""Exercise upgrades, rollback and retained data only in an explicit disposable kind cluster."""
import argparse
import hashlib
import json
import os
import pathlib
import socket
import subprocess
import tempfile
import time
import urllib.error
import urllib.request
import uuid

parser = argparse.ArgumentParser()
for option in ['context', 'namespace', 'chart', 'values', 'token-file', 'report']:
    parser.add_argument('--' + option, required=True)
parser.add_argument('--disposable', action='store_true')
args = parser.parse_args()
if not args.disposable or not args.context.startswith('kind-') or not args.namespace.startswith('sandherd-test'):
    parser.error('This destructive lifecycle test requires --disposable, a kind- context and a sandherd-test namespace.')
kube = ['kubectl', '--context', args.context, '-n', args.namespace]
helm = ['helm', '--kube-context', args.context, '-n', args.namespace]
token = pathlib.Path(args.token_file).read_text().strip()
forward = None
agents = []
volumes = {}
checks = []
release = 'sandherd'

def run(command, **kwargs):
    result = subprocess.run(command, capture_output=True, check=False, **kwargs)
    if result.returncode:
        raise RuntimeError('Command failed: ' + ' '.join(command[:5]) + '\n' + result.stderr.decode(errors='replace')[-2000:])
    return result.stdout

def resource(kind, name):
    return json.loads(run(kube + ['get', kind, name, '-o', 'json']))

def check(label, value):
    if not value:
        raise RuntimeError(label)
    checks.append(label)
    print(label, flush=True)

def connect():
    global forward, base
    if forward:
        forward.terminate()
        forward.wait(timeout=10)
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    forward = subprocess.Popen(kube + ['port-forward', 'service/sandherd-control-plane', str(port) + ':8080'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    base = 'http://127.0.0.1:' + str(port)
    for _ in range(100):
        try:
            urllib.request.urlopen(base + '/healthz', timeout=1).close()
            return
        except OSError:
            time.sleep(0.1)
    raise RuntimeError('API port forward unavailable')

def api(method, suffix='', data=None):
    request = urllib.request.Request(base + '/v1alpha1/agents' + suffix, method=method,
        data=json.dumps(data).encode() if data is not None else None,
        headers={'Authorization': 'Bearer ' + token, 'Content-Type': 'application/json', 'Idempotency-Key': str(uuid.uuid4())})
    with urllib.request.urlopen(request, timeout=30) as response:
        return json.load(response)

def wait_state(agent, state):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        value = api('GET', '/' + agent)
        if value['status']['state'] == state:
            return value
        if value['status']['state'] == 'failed':
            raise RuntimeError('Agent failed: ' + value['status'].get('reason', 'unknown'))
        time.sleep(1)
    raise RuntimeError('Agent state deadline')

def create(retention):
    agent = api('POST', data={'name': 'release-test-' + uuid.uuid4().hex[:10], 'spec': {
        'kind': 'shell', 'sandboxProfile': 'standard', 'resources': {'cpu': '500m', 'memory': '512Mi'},
        'workspace': {'size': '1Gi', 'storageProfile': 'default', 'retentionPolicy': retention},
        'lifecycle': {'idleTimeoutSeconds': 0}}})['id']
    agents.append(agent)
    wait_state(agent, 'running')
    pod = 'sandbox-' + agent.replace('-', '')
    pvc = 'workspace-' + pod
    volumes[pvc] = resource('pvc', pvc)['metadata']['uid']
    return agent, pod, pvc

def delete(agent):
    api('DELETE', '/' + agent)
    deadline = time.monotonic() + 120
    while time.monotonic() < deadline:
        try:
            api('GET', '/' + agent)
        except urllib.error.HTTPError as error:
            if error.code == 404:
                agents.remove(agent)
                return
            raise
        time.sleep(1)
    raise RuntimeError('Agent cleanup deadline')

try:
    connect()
    agent, pod, pvc = create('retain')
    canary = ('backup-canary-' + uuid.uuid4().hex + '\n').encode()
    run(kube + ['exec', '-i', pod, '-c', 'runner', '--', 'sh', '-c', 'cat > /workspace/repository/release-canary.txt'], input=canary)
    original_pod = resource('pod', pod)['metadata']['uid']
    original_pvc = volumes[pvc]
    backup = run(kube + ['exec', pod, '-c', 'runner', '--', 'tar', '-czf', '-', '-C', '/workspace/repository', 'release-canary.txt'])
    backup_hash = hashlib.sha256(backup).hexdigest()
    check('Quiescent repository backup created without credential or adapter-home data', bool(backup))
    # Probe an actual sandbox with its generated policy, not a separate unselected Pod.
    dns = subprocess.run(kube + ['exec', pod, '-c', 'runner', '--', 'nslookup', 'kubernetes.default.svc.cluster.local'], capture_output=True, timeout=15)
    if dns.returncode:
        print((dns.stdout + dns.stderr).decode(errors='replace')[-2000:], flush=True)
    check('Sandbox DNS permitted', dns.returncode == 0)
    api_ip = json.loads(run(['kubectl', '--context', args.context, '-n', 'default', 'get', 'service', 'kubernetes', '-o', 'json']))['spec']['clusterIP']
    probe = kube + ['exec', pod, '-c', 'runner', '--', 'timeout', '3', '/bin/bash', '-c', 'exec 3<>"/dev/tcp/$1/$2"', 'probe']
    allowed = subprocess.run(probe + ['kube-dns.kube-system.svc', '53'], capture_output=True, timeout=15)
    check('Sandbox TCP network probe succeeds against allowed DNS', allowed.returncode == 0)
    denied = subprocess.run(probe + [api_ip, '443'], capture_output=True, timeout=15)
    print('API TCP probe exit code: ' + str(denied.returncode), flush=True)
    # BusyBox timeout returns the terminating signal's shell status; GNU
    # timeout returns 124. The identical successful DNS probe validates tools.
    check('Sandbox Kubernetes API egress denied', denied.returncode in [1, 124, 137, 143])
    # No runtime template changes: the running Pod should survive both controller replacements.
    run(helm + ['upgrade', release, args.chart, '-f', args.values, '--set', 'controlPlane.resources.limits.cpu=600m', '--wait', '--timeout', '3m'], timeout=210)
    connect()
    wait_state(agent, 'running')
    check('Upgrade preserves running Pod identity', resource('pod', pod)['metadata']['uid'] == original_pod)
    check('Upgrade preserves PVC identity and repository data', resource('pvc', pvc)['metadata']['uid'] == original_pvc and run(kube + ['exec', pod, '-c', 'runner', '--', 'cat', '/workspace/repository/release-canary.txt']) == canary)
    run(helm + ['rollback', release, '1', '--wait', '--timeout', '3m'], timeout=210)
    connect()
    wait_state(agent, 'running')
    check('Rollback preserves running Pod and PVC identities', resource('pod', pod)['metadata']['uid'] == original_pod and resource('pvc', pvc)['metadata']['uid'] == original_pvc)
    check('Rollback preserves repository data', run(kube + ['exec', pod, '-c', 'runner', '--', 'cat', '/workspace/repository/release-canary.txt']) == canary)
    api('POST', '/' + agent + ':stop')
    wait_state(agent, 'stopped')
    delete(agent)
    check('Retain policy preserves workspace after Agent deletion', resource('pvc', pvc)['metadata']['uid'] == original_pvc)
    run(helm + ['uninstall', release, '--wait', '--timeout', '3m'], timeout=210)
    check('Uninstall preserves retained workspace', resource('pvc', pvc)['metadata']['uid'] == original_pvc)
    check('Uninstall preserves Agent CRD', bool(resource('crd', 'agents.sandherd.dev')))
    run(helm + ['install', release, args.chart, '-f', args.values, '--wait', '--timeout', '3m'], timeout=210)
    connect()
    restored, restored_pod, _ = create('delete')
    run(kube + ['exec', '-i', restored_pod, '-c', 'runner', '--', 'tar', '-xzf', '-', '-C', '/workspace/repository'], input=backup)
    check('Repository archive restores into a fresh isolated Agent', run(kube + ['exec', restored_pod, '-c', 'runner', '--', 'cat', '/workspace/repository/release-canary.txt']) == canary)
    delete(restored)
    report = {'context': args.context, 'namespace': args.namespace, 'chart': pathlib.Path(args.chart).name,
              'checks': checks, 'backupSHA256': backup_hash, 'originalPVCUID': original_pvc,
              'limitations': ['Tests a quiescent repository archive, not a CSI snapshot or application-consistent backup of an active model task.', 'Does not prove preservation across incompatible runtime-template or CRD changes.']}
    pathlib.Path(args.report).write_text(json.dumps(report, indent=2) + '\n')
finally:
    # These are only this script's test resources. UID checks protect unrelated data.
    for agent in list(agents):
        try:
            delete(agent)
        except Exception:
            print('Test Agent cleanup needs attention: ' + agent, flush=True)
    for name, uid in volumes.items():
        try:
            current = resource('pvc', name)
            if current['metadata']['uid'] == uid:
                run(kube + ['delete', 'pvc', name, '--wait=true', '--timeout=60s'])
        except Exception:
            pass
    if forward:
        forward.terminate()
        forward.wait(timeout=10)
