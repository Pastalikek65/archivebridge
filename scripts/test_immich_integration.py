"""Fault checks for the real-server integration harness (no Docker required)."""
import importlib.util
import json
import pathlib
import unittest

SPEC = importlib.util.spec_from_file_location('immich_integration', pathlib.Path(__file__).with_name('immich-integration.py'))
MODULE = importlib.util.module_from_spec(SPEC)
SPEC.loader.exec_module(MODULE)


def fake_docker_for_start(server, events, probe):
    identities = {'frontend': 'network-frontend'}

    def docker(args, **kwargs):
        if args[1] == 'create' and args[0] == 'network':
            name = args[-1]
            identity = 'network-frontend' if name.endswith('-frontend') else 'network-internal'
            events.append(('create-network', name))
            return identity
        if args[1] == 'create' and args[0] == 'volume':
            name = args[args.index('--name') + 1]
            events.append(('create-volume', name))
            return 'volume-' + name.rsplit('-', 1)[-1]
        if args[1] == 'create' and args[0] == 'container':
            name = args[args.index('--name') + 1]
            role = name.rsplit('-', 1)[-1]
            identity = 'container-' + role
            identities[role] = identity
            events.append(('create-container', role))
            return identity
        if args[:2] == ['container', 'start']:
            role = args[-1].rsplit('-', 1)[-1]
            events.append(('start', role))
            return ''
        if args[:2] == ['container', 'exec']:
            command = args[3:]
            role = 'postgres' if command and command[0] == 'pg_isready' else 'valkey'
            return probe(role, command, kwargs)
        if args[:2] == ['network', 'connect']:
            events.append(('connect', args[-1]))
            return ''
        if args[:2] == ['container', 'inspect']:
            identity = args[-1]
            if identity == identities.get('server'):
                return json.dumps([{
                    'NetworkSettings': {'Ports': {'2283/tcp': [{'HostIp': '127.0.0.1', 'HostPort': '49283'}]}},
                    'HostConfig': {'PortBindings': {'2283/tcp': [{'HostIp': '127.0.0.1', 'HostPort': ''}]},
                                   'NetworkMode': identities['frontend'], 'Privileged': False},
                    'Mounts': [{'Type': 'volume'}],
                }])
            return json.dumps([{
                'Config': {'Labels': {MODULE.OWNER_LABEL: server.owner}},
                'State': {'Status': 'running', 'ExitCode': 0, 'OOMKilled': False},
            }])
        if args[:3] == ['container', 'logs', '--tail']:
            return ''
        raise AssertionError(f'unexpected synthetic Docker command: {args!r}')

    return docker


class OwnershipTests(unittest.TestCase):
    def test_backends_retry_tcp_probes_before_server_starts(self):
        import unittest.mock
        events = []
        attempts = {'postgres': 0, 'valkey': 0}
        run = MODULE.OwnedServer()
        run.check_engine = lambda: None
        run.check_images = lambda: None

        def probe(role, command, kwargs):
            attempts[role] += 1
            events.append(('probe', role, attempts[role]))
            self.assertGreater(kwargs['timeout'], 0)
            self.assertLessEqual(kwargs['timeout'], 10)
            if role == 'postgres':
                self.assertEqual(command, ['pg_isready', '-h', '127.0.0.1', '-U', 'postgres',
                                            '-d', 'immich', '-t', '1'])
                if attempts[role] == 1:
                    raise MODULE.Failure('DOCKER_COMMAND_FAILED')
                return '127.0.0.1:5432 - accepting connections'
            self.assertEqual(command, ['valkey-cli', '-h', '127.0.0.1', 'ping'])
            return 'LOADING' if attempts[role] == 1 else 'PONG'

        run.docker = fake_docker_for_start(run, events, probe)
        with unittest.mock.patch.object(MODULE.time, 'sleep', return_value=None), \
             unittest.mock.patch.object(MODULE, 'http_json', return_value={
                 'major': 3, 'minor': 3, 'patch': 1, 'prerelease': None,
             }):
            run.start()

        server_start = events.index(('start', 'server'))
        self.assertLess(events.index(('start', 'db')), events.index(('probe', 'postgres', 1)))
        self.assertLess(events.index(('start', 'redis')), events.index(('probe', 'valkey', 1)))
        self.assertLess(events.index(('probe', 'postgres', 2)), server_start)
        self.assertLess(events.index(('probe', 'valkey', 2)), server_start)
        self.assertEqual(run.backend_diagnostics, [
            {'service': 'postgres', 'state': 'ready'}, {'service': 'valkey', 'state': 'ready'},
        ])

    def test_backend_deadline_captures_owned_diagnostics_without_starting_server(self):
        import unittest.mock
        events = []
        run = MODULE.OwnedServer()
        run.check_engine = lambda: None
        run.check_images = lambda: None
        clock = {'now': 0}

        def probe(role, command, kwargs):
            events.append(('probe', role, command))
            self.assertGreater(kwargs['timeout'], 0)
            self.assertLessEqual(kwargs['timeout'], 10)
            raise MODULE.Failure('DOCKER_COMMAND_FAILED')

        def sleep(seconds):
            clock['now'] += seconds

        run.docker = fake_docker_for_start(run, events, probe)
        with unittest.mock.patch.object(MODULE.time, 'monotonic', side_effect=lambda: clock['now']), \
             unittest.mock.patch.object(MODULE.time, 'sleep', side_effect=sleep):
            with self.assertRaises(MODULE.Failure) as error:
                run.start()

        self.assertEqual(str(error.exception), 'BACKEND_START_TIMEOUT')
        self.assertEqual(clock['now'], 60)
        self.assertEqual([event[1] for event in events if event[0] == 'start'], ['db', 'redis'])
        self.assertNotIn(('create-container', 'server'), events)
        self.assertTrue(any(event[0] == 'probe' and event[1] == 'postgres'
                            and event[2][event[2].index('-h') + 1] == '127.0.0.1' for event in events))
        self.assertTrue(any(event[0] == 'probe' and event[1] == 'valkey'
                            and event[2][event[2].index('-h') + 1] == '127.0.0.1' for event in events))
        self.assertEqual(run.backend_diagnostics, [
            {'service': 'postgres', 'state': 'not_ready'}, {'service': 'valkey', 'state': 'not_ready'},
        ])
        self.assertEqual(len(run.startup_diagnostics), 2)
        self.assertTrue(all(item['state'] == 'running' for item in run.startup_diagnostics))

    def test_startup_diagnostics_keep_credentials_and_foreign_logs_out(self):
        import json
        run = MODULE.OwnedServer()
        run.created = [('container', 'owned-container'), ('container', 'foreign-container')]
        calls = []
        def docker(args, **kwargs):
            calls.append(args)
            if args[1] == 'inspect':
                owner = run.owner if args[-1] == 'owned-container' else 'another-owner'
                return json.dumps([{'Config': {'Labels': {MODULE.OWNER_LABEL: owner}, 'Env': ['DB_PASSWORD=synthetic-secret']},
                                    'State': {'Status': 'exited', 'ExitCode': 1, 'OOMKilled': False,
                                              'Error': 'synthetic-secret', 'Health': {'Status': 'unhealthy', 'Log': ['synthetic-secret']}}}])
            self.assertEqual(args, ['container', 'logs', '--tail', '80', 'owned-container'])
            return 'EAI_AGAIN database password=synthetic-secret'
        run.docker = docker
        run.capture_startup_diagnostics()
        self.assertEqual(run.startup_diagnostics[0]['logCategories'], ['dns'])
        self.assertEqual(run.startup_diagnostics[0]['state'], 'exited')
        self.assertEqual(run.startup_diagnostics[1], {'identity': 'foreign-container', 'inspection': 'failed'})
        self.assertNotIn('synthetic-secret', json.dumps(run.startup_diagnostics))
        self.assertEqual(len(calls), 3)

    def test_log_category_reader_includes_stderr(self):
        import subprocess
        import unittest.mock
        result = subprocess.CompletedProcess([], 0, stdout=b'normal startup', stderr=b'ECONNREFUSED')
        with unittest.mock.patch.object(MODULE.subprocess, 'run', return_value=result):
            self.assertIn('ECONNREFUSED', MODULE.docker_command(['container', 'logs', '--tail', '80', 'owned-container']))

    def test_conflicting_exif_fixture_is_fixed_original_with_separate_takeout_date(self):
        import hashlib
        import json
        import tempfile
        import zipfile
        with tempfile.TemporaryDirectory() as directory:
            source = MODULE.conflicting_exif_fixture(pathlib.Path(directory))
            with zipfile.ZipFile(source) as archive:
                media = archive.read('Takeout/Google Photos/Source date precedence/edited.jpg')
                metadata = json.loads(archive.read('Takeout/Google Photos/Source date precedence/edited.jpg.json'))
        self.assertEqual(len(media), 1764)
        self.assertEqual(hashlib.sha256(media).hexdigest(), '26875c6fd8f73fffc3cc259086fa808ec22003ee20d5e69e9df1335a0d0ba058')
        self.assertIn(b'2020:01:02 03:04:05', media)
        self.assertEqual(metadata['photoTakenTime']['timestamp'], '1700000000')
        self.assertEqual(MODULE.FIXTURE_DATES[hashlib.sha256(media).hexdigest()], '2023-11-14T22:13:20Z')

    def test_processed_dimensions_and_placeholder_flag_cannot_hide_exif_date_mismatch(self):
        import unittest.mock
        sha = next(iter(MODULE.FIXTURE_DATES))
        date = MODULE.FIXTURE_DATES[sha]
        report = {'contents': [{'sha256': sha, 'remoteAssetId': 'synthetic-id', 'state': 'uploaded'}],
                  'files': [{'sha256': sha, 'date': date, 'state': 'uploaded'}]}
        detail = {'fileCreatedAt': date, 'hasMetadata': True,
                  'exifInfo': {'exifImageWidth': 320, 'exifImageHeight': 200, 'dateTimeOriginal': '2020-01-02T03:04:05+00:00'}}
        server = type('SyntheticServer', (), {'origin': 'http://127.0.0.1:1234'})()
        with unittest.mock.patch.object(MODULE, 'http_json', return_value=detail):
            with self.assertRaises(MODULE.Failure) as error:
                MODULE.wait_processed_metadata(server, 'key', report)
        self.assertEqual(str(error.exception), 'POST_PROCESSING_EXIF_DATE_CHANGED')

    def test_metadata_ground_truth_cannot_follow_an_incorrect_report_date(self):
        import unittest.mock
        sha = next(iter(MODULE.FIXTURE_DATES))
        report = {'contents': [{'sha256': sha, 'remoteAssetId': 'synthetic-id', 'state': 'uploaded'}],
                  'files': [{'sha256': sha, 'date': '2024-01-01T00:00:00Z', 'state': 'uploaded'}]}
        with unittest.mock.patch.object(MODULE, 'http_json', side_effect=AssertionError('Do not query before ground truth check')):
            with self.assertRaises(MODULE.Failure) as error:
                MODULE.wait_processed_metadata(object(), 'key', report)
        self.assertEqual(str(error.exception), 'REPORT_DATE_DIFFERS_FROM_KNOWN_FIXTURE')

    def test_context_cannot_override_approved_docker_host(self):
        import os
        import unittest.mock
        run = MODULE.OwnedServer(docker=lambda *a, **k: self.fail('No Docker call allowed for ambiguous endpoint overrides'))
        with unittest.mock.patch.dict(os.environ, {'DOCKER_HOST': 'unix:///var/run/docker.sock', 'DOCKER_CONTEXT': 'remote'}):
            with self.assertRaises(MODULE.Failure) as error:
                run.start()
        self.assertEqual(str(error.exception), 'DOCKER_ENDPOINT_OVERRIDES_AMBIGUOUS')
        self.assertEqual(run.created, [])

    def test_remote_context_rejected_before_any_resource_creation(self):
        import os
        import unittest.mock
        calls = []
        def docker(args, **kwargs):
            calls.append(args)
            if args == ['context', 'show']:
                return 'selected-remote'
            if args[:2] == ['context', 'inspect']:
                return '[{"Endpoints":{"docker":{"Host":"ssh://remote.example.invalid"}}}]'
            self.fail('No image access or creation allowed for a remote context')
        run = MODULE.OwnedServer(docker=docker)
        with unittest.mock.patch.dict(os.environ, {'DOCKER_HOST': ''}), self.assertRaises(MODULE.Failure):
            run.start()
        self.assertEqual(run.created, [])
        self.assertEqual(len(calls), 2)

    def test_local_endpoint_boundary(self):
        for endpoint in ['unix:///var/run/docker.sock', 'unix:///run/user/1000/docker.sock', 'npipe:////./pipe/dockerDesktopLinuxEngine']:
            MODULE.validate_local_endpoint(endpoint)
        for endpoint in ['tcp://127.0.0.1:2375', 'ssh://remote', 'npipe:////other/pipe/docker_engine', 'unix:///arbitrary-forward.sock']:
            with self.subTest(endpoint=endpoint), self.assertRaises(MODULE.Failure):
                MODULE.validate_local_endpoint(endpoint)

    def test_create_response_lost_still_cleans_exact_owned_name(self):
        import json
        calls = []
        run = MODULE.OwnedServer()
        name = run.prefix + '-net'
        def docker(args, **kwargs):
            calls.append(args)
            if args[1] == 'create':
                raise MODULE.Failure('DOCKER_EXECUTION_FAILED')
            if args[1] == 'inspect':
                return json.dumps([{'Id': 'daemon-created-id', 'Labels': {MODULE.OWNER_LABEL: run.owner}}])
            return ''
        run.docker = docker
        with self.assertRaises(MODULE.Failure):
            run.create('network', ['--internal', name])
        self.assertEqual(run.created, [('network', name)])
        run.cleanup()
        self.assertEqual(calls[-1], ['network', 'rm', name])

    def test_actual_version_dto_requires_null_prerelease(self):
        MODULE.check_version({'major': 3, 'minor': 3, 'patch': 1, 'prerelease': None})
        for item in [{'major': 3, 'minor': 3, 'patch': 1}, {'major': 3, 'minor': 3, 'patch': 1, 'prerelease': 1},
                     {'major': 3, 'minor': 3, 'patch': True, 'prerelease': None}]:
            with self.subTest(item=item), self.assertRaises(MODULE.Failure):
                MODULE.check_version(item)

    def test_cleanup_refuses_changed_owner_and_uses_created_ids(self):
        calls = []
        def docker(args, **kwargs):
            calls.append(args)
            if args[:2] == ['container', 'inspect']:
                return '[{"Id":"owned-id","Config":{"Labels":{"org.archivebridge.integration":"foreign"}}}]'
            self.fail('Removal must not run after ownership mismatch')
        run = MODULE.OwnedServer(docker=docker)
        run.created.append(('container', 'owned-id'))
        with self.assertRaises(MODULE.Failure):
            run.cleanup()
        self.assertEqual(calls[0][-1], 'owned-id')

    def test_cleanup_in_reverse_order_only_after_exact_owner(self):
        calls = []
        run = MODULE.OwnedServer()
        def docker(args, **kwargs):
            calls.append(args)
            if args[1] == 'inspect':
                import json
                item = {'Id': args[-1], 'Labels': {MODULE.OWNER_LABEL: run.owner}}
                if args[0] == 'container':
                    item['Config'] = {'Labels': item.pop('Labels')}
                return json.dumps([item])
            return ''
        run.docker = docker
        run.created = [('network', 'net-id'), ('volume', 'vol-id'), ('container', 'ctr-id')]
        run.cleanup()
        removed = [(x[0], x[-1]) for x in calls if x[1] == 'rm']
        self.assertEqual(removed, [('container', 'ctr-id'), ('volume', 'vol-id'), ('network', 'net-id')])
        self.assertEqual(run.created, [])

    def test_http_errors_never_echo_server_or_key(self):
        import unittest.mock
        import urllib.error
        secret = 'SECRET-CANARY'
        with unittest.mock.patch.object(MODULE.urllib.request.OpenerDirector, 'open', side_effect=urllib.error.HTTPError('http://secret', 403, secret, {}, None)):
            with self.assertRaises(MODULE.Failure) as error:
                MODULE.http_json('http://127.0.0.1:1234', '/users/me', key=secret)
        self.assertEqual(str(error.exception), 'HTTP_STATUS_403')
        self.assertNotIn(secret, str(error.exception))

    def test_inventory_requires_pinned_linux_images(self):
        run = MODULE.OwnedServer(docker=lambda *a, **k: '[{"Os":"windows","Architecture":"amd64","RepoDigests":[]}]')
        with self.assertRaises(MODULE.Failure):
            run.check_images()

    def test_url_credentials_and_nonloopback_rejected(self):
        for origin in ['http://localhost:2283', 'http://user:pass@127.0.0.1:2283', 'https://example.com']:
            with self.subTest(origin=origin), self.assertRaises(MODULE.Failure):
                MODULE.http_json(origin, '/server/version')


if __name__ == '__main__':
    unittest.main()
