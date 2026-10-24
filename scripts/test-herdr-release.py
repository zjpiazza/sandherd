#!/usr/bin/env python3
"""Test packaged Herdr over SSH using only a dedicated disposable installation."""
import argparse
import json
import fcntl
import os
import pathlib
import pty
import select
import shlex
import socket
import struct
import subprocess
import tempfile
import termios
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
    parser.error('Requires --disposable, a kind- context and a sandherd-test namespace.')
kube = ['kubectl', '--context', args.context, '-n', args.namespace]
helm = ['helm', '--kube-context', args.context, '-n', args.namespace]
token = pathlib.Path(args.token_file).read_text().strip()
test_id = uuid.uuid4().hex[:12]
name = 'herdr-test-' + test_id
secret = 'herdr-access-' + test_id
checks, forwards = [], []
agent = workspace = previous_revision = home_uid = workspace_uid = None
upgraded = secret_created = False
base = ''


def run(command, **kwargs):
    result = subprocess.run(command, capture_output=True, timeout=kwargs.pop('timeout', 40), **kwargs)
    if result.returncode:
        raise RuntimeError('Command failed: ' + ' '.join(command[:5]) + '\n' + result.stderr.decode(errors='replace')[-2000:])
    return result.stdout


def check(label, value):
    if not value:
        raise RuntimeError(label)
    checks.append(label)
    print(label, flush=True)


def forward(service, remote_port):
    with socket.socket() as listener:
        listener.bind(('127.0.0.1', 0))
        port = listener.getsockname()[1]
    process = subprocess.Popen(kube + ['port-forward', 'service/' + service, str(port) + ':' + str(remote_port)],
                               stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL)
    forwards.append(process)
    for _ in range(100):
        try:
            with socket.create_connection(('127.0.0.1', port), timeout=.2):
                return port
        except OSError:
            time.sleep(.1)
    raise RuntimeError('Port forward unavailable: ' + service)


def connect_api():
    global base
    base = 'http://127.0.0.1:' + str(forward('sandherd-control-plane', 8080))


def api(method, suffix=''):
    request = urllib.request.Request(base + '/v1alpha1/agents' + suffix, method=method,
        headers={'Authorization': 'Bearer ' + token, 'Idempotency-Key': str(uuid.uuid4())})
    with urllib.request.urlopen(request, timeout=20) as response:
        return json.load(response)


def wait_state(state):
    deadline = time.monotonic() + 180
    while time.monotonic() < deadline:
        value = api('GET', '/' + agent)
        if value['status']['state'] == state:
            return value
        if value['status']['state'] == 'failed':
            raise RuntimeError('Agent failed: ' + value['status'].get('reason', 'unknown'))
        time.sleep(1)
    raise RuntimeError('Agent did not reach ' + state)


def resource(kind, resource_name):
    return json.loads(run(kube + ['get', kind, resource_name, '-o', 'json']))


def remote(arguments, **kwargs):
    return run(ssh + [shlex.join(arguments)], **kwargs)


def herdr(*arguments):
    output = remote(['herdr', *arguments])
    # Mutations such as pane run may return no body; pane reads return text.
    if output.lstrip().startswith(b'{'):
        return json.loads(output)['result']
    return output.decode()


def manager(action, answer):
    return remote(['env', 'SANDHERD_ACTION=' + action, 'HERDR_WORKSPACE_ID=' + workspace,
        'HERDR_PLUGIN_STATE_DIR=/home/sandherd/.local/state/herdr/plugins/dev.sandherd',
        'SANDHERD_AGENT_PLACEMENT=tab', 'sandherd-herdr-bridge', 'manager'], input=answer.encode())


def bridge_state():
    return json.loads(remote(['cat', '/home/sandherd/.local/state/herdr/plugins/dev.sandherd/agents/' + agent + '.json']))


def attached_pane(previous=None):
    deadline = time.monotonic() + 90
    while time.monotonic() < deadline:
        state = bridge_state()
        if state.get('paneId') and state['paneId'] != previous and state.get('runnerGeneration'):
            return state['paneId']
        time.sleep(.2)
    raise RuntimeError('Bridge did not attach')


def command(pane, text, marker):
    herdr('pane', 'run', pane, text)
    herdr('pane', 'wait-output', pane, '--match', marker, '--timeout', '30000')


with tempfile.TemporaryDirectory(prefix='sandherd-herdr-') as temporary:
    directory = pathlib.Path(temporary)
    try:
        connect_api()
        if api('GET')['items']:
            raise RuntimeError('Herdr acceptance requires an installation without existing Agents.')
        previous_values = json.loads(run(helm + ['get', 'values', 'sandherd', '-o', 'json'])) or {}
        if previous_values.get('herdr', {}).get('enabled'):
            raise RuntimeError('Refusing to replace an existing enabled Herdr installation.')
        if run(kube + ['get', 'pvc', 'sandherd-herdr-home', '--ignore-not-found', '-o', 'name']).strip():
            raise RuntimeError('Refusing to reuse or delete an existing retained Herdr home volume.')
        previous_revision = str(json.loads(run(helm + ['history', 'sandherd', '-o', 'json']))[-1]['revision'])
        for key in ['client', 'host']:
            run(['ssh-keygen', '-q', '-t', 'ed25519', '-N', '', '-f', str(directory / key)])
        run(kube + ['create', 'secret', 'generic', secret,
            '--from-file=authorized_keys=' + str(directory / 'client.pub'),
            '--from-file=ssh_host_ed25519_key=' + str(directory / 'host'),
            '--from-file=api-token=' + args.token_file])
        secret_created = True
        # Mark before upgrade so a partially failed release is rolled back too.
        upgraded = True
        run(helm + ['upgrade', 'sandherd', args.chart, '-f', args.values,
            '--set', 'herdr.enabled=true', '--set', 'herdr.agentKind=shell',
            '--set', 'herdr.accessSecret=' + secret, '--wait', '--timeout', '4m'], timeout=270)
        home_uid = resource('pvc', 'sandherd-herdr-home')['metadata']['uid']
        connect_api()
        port = forward('sandherd-herdr', 22)
        host_public = (directory / 'host.pub').read_text().split()
        (directory / 'known_hosts').write_text('[127.0.0.1]:' + str(port) + ' ' + ' '.join(host_public[:2]) + '\n')
        ssh = ['ssh', '-p', str(port), '-i', str(directory / 'client'),
            '-o', 'BatchMode=yes', '-o', 'IdentitiesOnly=yes', '-o', 'StrictHostKeyChecking=yes',
            '-o', 'UserKnownHostsFile=' + str(directory / 'known_hosts'),
            '-o', 'ConnectTimeout=5', '-o', 'LogLevel=ERROR', 'sandherd@127.0.0.1']
        check('SSH uses the pinned host key and non-root public-key login', remote(['id', '-u']).strip() == b'65532')
        plugins = herdr('plugin', 'list', '--json')['plugins']
        check('Prebuilt Sandherd plugin is installed and enabled', any(p['plugin_id'] == 'dev.sandherd' and p['enabled'] for p in plugins))
        created = herdr('workspace', 'create', '--label', name, '--no-focus')
        workspace = created['workspace']['workspace_id']
        manager('create', name + '\n')
        matches = [a for a in api('GET')['items'] if a['name'] == name]
        check('Herdr manager creates an Agent through the public API', len(matches) == 1)
        agent = matches[0]['id']
        wait_state('running')
        pod = 'sandbox-' + agent.replace('-', '')
        workspace_pvc = 'workspace-' + pod
        workspace_uid = resource('pvc', workspace_pvc)['metadata']['uid']
        original_pod = resource('pod', pod)['metadata']['uid']
        pane = attached_pane()
        marker = 'HERDR_READY_' + test_id
        command(pane, "printf '%s' " + test_id + " > /workspace/repository/herdr-canary; printf '%s%s\\n' HERDR_READY_ " + test_id, marker)
        check('Herdr plugin pane sends input and receives sandbox output', True)
        master, slave = pty.openpty()
        try:
            fcntl.ioctl(slave, termios.TIOCSWINSZ, struct.pack('HHHH', 40, 160, 0, 0))
            client = subprocess.Popen(ssh[:-1] + ['-tt', ssh[-1], 'TERM=xterm-256color herdr --session default'],
                                      stdin=slave, stdout=slave, stderr=slave)
            try:
                output = bytearray()
                deadline = time.monotonic() + 3
                while time.monotonic() < deadline and client.poll() is None:
                    if select.select([master], [], [], .1)[0]:
                        output.extend(os.read(master, 65536))
                attached = client.poll() is None and len(output) > 0
                if not attached:
                    print('Test SSH client failed: ' + repr(bytes(output[-1500:])), flush=True)
                check('Remote Herdr terminal client attaches over SSH', attached)
            finally:
                client.terminate()
                client.wait(timeout=10)
        finally:
            os.close(master)
            os.close(slave)
        check('SSH client disconnect preserves the running sandbox', resource('pod', pod)['metadata']['uid'] == original_pod and api('GET', '/' + agent)['status']['state'] == 'running')
        generation = bridge_state()['runnerGeneration']
        herdr('pane', 'run', pane, "(sleep 3; printf '%s%s\\n' HERDR_REPLAY_ " + test_id + ') &')
        herdr('plugin', 'pane', 'close', pane)
        time.sleep(4)
        manager('attach', '1\n')
        pane = attached_pane(pane)
        herdr('pane', 'wait-output', pane, '--match', 'HERDR_REPLAY_' + test_id, '--timeout', '30000')
        check('Replacement Herdr pane replays missed output without restarting the runner', bridge_state()['runnerGeneration'] == generation and resource('pod', pod)['metadata']['uid'] == original_pod)
        manager('stop', '1\n')
        wait_state('stopped')
        manager('resume', '1\n')
        wait_state('running')
        # Herdr removes a plugin pane when the stopped bridge exits. Close it
        # only if it survived the stop, before asking the manager to reattach.
        if any(p['pane_id'] == pane for p in herdr('pane', 'list', '--workspace', workspace)['panes']):
            herdr('pane', 'close', pane)
        manager('attach', '1\n')
        pane = attached_pane(pane)
        command(pane, "printf '%s' HERDR_PERSISTED_; cat /workspace/repository/herdr-canary; printf '\\n'", 'HERDR_PERSISTED_' + test_id)
        check('Herdr stop/resume preserves workspace contents and PVC identity', resource('pvc', workspace_pvc)['metadata']['uid'] == workspace_uid)
        manager('delete', '1\n' + name + '\n')
        deadline = time.monotonic() + 120
        while time.monotonic() < deadline:
            try:
                api('GET', '/' + agent)
            except urllib.error.HTTPError as error:
                if error.code == 404:
                    break
                raise
            time.sleep(1)
        else:
            raise RuntimeError('Herdr deletion did not complete')
        check('Herdr deletion honors the retained workspace policy', resource('pvc', workspace_pvc)['metadata']['uid'] == workspace_uid)
        report = {'context': args.context, 'namespace': args.namespace, 'checks': checks,
            'herdrVersion': remote(['herdr', '--version']).decode().strip(),
            'gatewayImage': resource('deployment', 'sandherd-herdr')['spec']['template']['spec']['containers'][0]['image'],
            'limitations': ['Shell adapter acceptance; subscription login and native model session acceptance are separate.', 'SSH port-forward transport; homelab Tailscale routing has separate acceptance evidence.']}
    finally:
        # Every name here was generated by this run; never delete a caller's data.
        failures = []
        if base:
            try:
                for candidate in api('GET')['items']:
                    if candidate['name'] == name:
                        api('DELETE', '/' + candidate['id'])
            except Exception:
                failures.append('Agent cleanup')
        if workspace:
            try:
                herdr('workspace', 'close', workspace)
            except Exception:
                failures.append('Herdr test workspace cleanup')
        if upgraded:
            try:
                run(helm + ['rollback', 'sandherd', previous_revision, '--wait', '--timeout', '3m'], timeout=210)
            except Exception:
                failures.append('Helm rollback')
        if secret_created:
            try:
                run(kube + ['delete', 'secret', secret, '--ignore-not-found'])
            except Exception:
                failures.append('Test credential cleanup')
        for volume, uid in [('sandherd-herdr-home', home_uid), (locals().get('workspace_pvc', ''), workspace_uid)]:
            if uid:
                try:
                    if resource('pvc', volume)['metadata']['uid'] != uid:
                        raise RuntimeError('PVC identity changed')
                    run(kube + ['delete', 'pvc', volume, '--wait', '--timeout=90s'], timeout=100)
                except Exception:
                    failures.append('Test volume cleanup: ' + volume)
        for process in forwards:
            process.terminate()
            process.wait(timeout=10)
        if failures:
            raise RuntimeError('; '.join(failures))
    check('Test credentials, Herdr workspace and temporary volumes cleaned up', True)
    pathlib.Path(args.report).write_text(json.dumps(report, indent=2) + '\n')
