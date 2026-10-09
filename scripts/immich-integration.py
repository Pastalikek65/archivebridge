#!/usr/bin/env python3
"""Exercise a native ArchiveBridge against an owned, pinned Immich 3.3.1.

Requires a Linux Docker engine (WSL2 on Windows). Uses synthetic fixtures only;
credentials remain in memory. No existing server, account, volume, or network
is used. Database/cache use an internal network; the server also joins an
owned ordinary bridge so Docker can publish an ephemeral IPv4 loopback port.
The server bridge is not an outbound-traffic sandbox. Docker
images are deliberately not pulled here: provision the exact digests first.
"""
from __future__ import annotations

import argparse
import base64
import contextlib
import datetime as dt
import hashlib
import http.client
import http.server
import importlib.util
import io
import json
import os
from pathlib import Path
import platform
import re
import secrets
import subprocess
import sys
import threading
import time
import urllib.error
import urllib.parse
import urllib.request
import uuid
import zipfile

ROOT = Path(__file__).resolve().parents[1]
OWNER_LABEL = 'org.archivebridge.integration'
IMAGES = {
    'server': 'ghcr.io/immich-app/immich-server@sha256:96c6ec77cdea5a3c90539b81baf8c276b834deb595b1603c4aefc1903ab6a05b',
    'database': 'ghcr.io/immich-app/postgres@sha256:bcf63357191b76a916ae5eb93464d65c07511da41e3bf7a8416db519b40b1c23',
    'redis': 'valkey/valkey@sha256:c123e3715db63d06d4ad6964884037aa0d5d4d703939b9929954112889708e1d',
}
PERMISSIONS = ['asset.upload', 'asset.read', 'asset.download', 'album.read', 'album.create', 'albumAsset.create', 'user.read']
KEY_ENV = 'ARCHIVEBRIDGE_INTEGRATION_KEY'
FIXTURE_DATES = {
    '26875c6fd8f73fffc3cc259086fa808ec22003ee20d5e69e9df1335a0d0ba058': '2023-11-14T22:13:20Z',
    '83807b4ef61c1d03497a389abee15f67f36fbc10506f767fdd69c1a044594153': '2023-11-14T22:13:20Z',
    'f9454317f83e5d8ab90431433bbe6a345f69f5d213138756db22e825168d034c': '2023-11-15T22:13:20Z',
    'e898e7ad061e3e93536b44daba1ee0a5d1b548a42d3fe32f817978257f711109': '2023-11-14T22:13:19Z',
    '932e3e709ffde373c75b1d6724b5d005aea9d5736c1bd119be867014a88f511c': '2023-11-14T22:13:20Z',
}


# Synthetic solid-color 320x200 JPEG with DateTimeOriginal 2020-01-02 UTC.
# Generated once for the independent source-date regression; original bytes are
# fixed and hash-checked. No Pillow/external image library is required to run.
CONFLICTING_EXIF_JPEG = base64.b64decode(
    '/9j/4AAQSkZJRgABAQAAAQABAAD/4QBcRXhpZgAATU0AKgAAAAgAAYdpAAQAAAABAAAAGgAAAAAAApADAAIAAAAUAAAAOJARAAIA'
    'AAAHAAAATAAAAAAyMDIwOjAxOjAyIDAzOjA0OjA1ACswMDowMAAA/9sAQwADAgIDAgIDAwMDBAMDBAUIBQUEBAUKBwcGCAwKDAwL'
    'CgsLDQ4SEA0OEQ4LCxAWEBETFBUVFQwPFxgWFBgSFBUU/9sAQwEDBAQFBAUJBQUJFA0LDRQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU'
    'FBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQUFBQU/8AAEQgAyAFAAwEiAAIRAQMRAf/EAB8AAAEFAQEBAQEBAAAAAAAAAAABAgMEBQYH'
    'CAkKC//EALUQAAIBAwMCBAMFBQQEAAABfQECAwAEEQUSITFBBhNRYQcicRQygZGhCCNCscEVUtHwJDNicoIJChYXGBkaJSYnKCkq'
    'NDU2Nzg5OkNERUZHSElKU1RVVldYWVpjZGVmZ2hpanN0dXZ3eHl6g4SFhoeIiYqSk5SVlpeYmZqio6Slpqeoqaqys7S1tre4ubrC'
    'w8TFxsfIycrS09TV1tfY2drh4uPk5ebn6Onq8fLz9PX29/j5+v/EAB8BAAMBAQEBAQEBAQEAAAAAAAABAgMEBQYHCAkKC//EALUR'
    'AAIBAgQEAwQHBQQEAAECdwABAgMRBAUhMQYSQVEHYXETIjKBCBRCkaGxwQkjM1LwFWJy0QoWJDThJfEXGBkaJicoKSo1Njc4OTpD'
    'REVGR0hJSlNUVVZXWFlaY2RlZmdoaWpzdHV2d3h5eoKDhIWGh4iJipKTlJWWl5iZmqKjpKWmp6ipqrKztLW2t7i5usLDxMXGx8jJ'
    'ytLT1NXW19jZ2uLj5OXm5+jp6vLz9PX29/j5+v/aAAwDAQACEQMRAD8A8iooor9dPw8KKKKACiiigAooooAKKKKACiiigAooooAK'
    'KKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKA'
    'CiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiii'
    'gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAoo'
    'ooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAK'
    'KKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKA'
    'CiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiii'
    'gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAoo'
    'ooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAK'
    'KKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKA'
    'CiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiii'
    'gAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAoo'
    'ooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAK'
    'KKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKA'
    'CiiigAooooAKKKKACiiigAooooAKKKKACiiigAooooAKKKKAP//Z'
    , validate=True)


class Failure(Exception):
    """Static diagnostic only: never include transport errors or secrets."""


def require(condition, code):
    if not condition:
        raise Failure(code)


def check_version(version):
    require(isinstance(version, dict) and set(version) == {'major', 'minor', 'patch', 'prerelease'}
            and all(type(version[x]) is int for x in ('major', 'minor', 'patch'))
            and (version['major'], version['minor'], version['patch']) == (3, 3, 1)
            and version['prerelease'] is None, 'SERVER_VERSION_MISMATCH')


def docker_command(args, env=None, timeout=180):
    try:
        result = subprocess.run([os.environ.get('ARCHIVEBRIDGE_DOCKER', 'docker'), *args],
                                env=env, capture_output=True, timeout=timeout, check=False)
    except (OSError, subprocess.TimeoutExpired):
        raise Failure('DOCKER_EXECUTION_FAILED') from None
    require(result.returncode == 0, 'DOCKER_COMMAND_FAILED')
    if args[:2] == ['container', 'logs']:
        return (result.stdout + result.stderr).decode('utf-8', errors='replace')
    return result.stdout.decode('utf-8', errors='strict').strip()


class NoRedirect(urllib.request.HTTPRedirectHandler):
    def redirect_request(self, request, fp, code, msg, headers, newurl):
        return None


def http_json(origin, route, method='GET', body=None, key=None, bearer=None):
    parsed = urllib.parse.urlsplit(origin)
    require(parsed.scheme == 'http' and parsed.hostname == '127.0.0.1' and parsed.port
            and not parsed.username and not parsed.password and parsed.path in ('', '/')
            and not parsed.query and not parsed.fragment, 'HARNESS_ORIGIN_INVALID')
    require(route.startswith('/') and not route.startswith('//'), 'HARNESS_ROUTE_INVALID')
    headers = {'Accept': 'application/json', 'Content-Type': 'application/json'}
    if key:
        headers['x-api-key'] = key
    if bearer:
        headers['Authorization'] = 'Bearer ' + bearer
    payload = None if body is None else json.dumps(body, ensure_ascii=False).encode('utf-8')
    request = urllib.request.Request(origin + '/api' + route, data=payload, method=method, headers=headers)
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    try:
        with opener.open(request, timeout=20) as response:
            raw = response.read(8 * 1024 * 1024 + 1)
            require(len(raw) <= 8 * 1024 * 1024, 'HARNESS_RESPONSE_TOO_LARGE')
            return json.loads(raw)
    except urllib.error.HTTPError as error:
        raise Failure('HTTP_STATUS_' + str(error.code)) from None
    except (urllib.error.URLError, TimeoutError, OSError, ValueError):
        raise Failure('HTTP_REQUEST_FAILED') from None


class OwnedServer:
    def __init__(self, docker=docker_command):
        self.owner = uuid.uuid4().hex
        self.prefix = 'archivebridge-it-' + self.owner
        self.docker = docker
        self.created = []
        self.origin = None
        self.inventory = {}
        self.boundary = {}
        self.engine = {}
        self.startup_diagnostics = []
        self.backend_diagnostics = []

    def check_engine(self):
        # The ambient context can otherwise target an unrelated remote engine.
        # Validate DOCKER_HOST's override separately from context metadata.
        override = os.environ.get('DOCKER_HOST', '')
        require(not (override and os.environ.get('DOCKER_CONTEXT', '')), 'DOCKER_ENDPOINT_OVERRIDES_AMBIGUOUS')
        if override:
            validate_local_endpoint(override)
            endpoint = override
        else:
            context = self.docker(['context', 'show'])
            require(context and '\n' not in context, 'DOCKER_CONTEXT_INVALID')
            metadata = json.loads(self.docker(['context', 'inspect', context]))
            require(len(metadata) == 1, 'DOCKER_CONTEXT_INVALID')
            endpoint = metadata[0].get('Endpoints', {}).get('docker', {}).get('Host', '')
            validate_local_endpoint(endpoint)
        info = json.loads(self.docker(['info', '--format', '{{json .}}']))
        require(info.get('OSType') == 'linux' and info.get('Architecture') in ('x86_64', 'amd64'), 'LINUX_X64_DOCKER_REQUIRED')
        self.engine = {'endpoint': endpoint, 'osType': info['OSType'], 'architecture': info['Architecture'],
                       'serverVersion': info.get('ServerVersion'), 'operatingSystem': info.get('OperatingSystem')}

    def check_images(self):
        for name, image in IMAGES.items():
            info = json.loads(self.docker(['image', 'inspect', image]))
            require(len(info) == 1 and info[0].get('Os') == 'linux'
                    and info[0].get('Architecture') == 'amd64'
                    and image in info[0].get('RepoDigests', []), 'IMAGE_PIN_OR_PLATFORM_MISMATCH')
            self.inventory[name] = {'reference': image, 'id': info[0]['Id']}

    def create(self, kind, args, env=None):
        # Record the unique name before calling Docker: a daemon-side create
        # can succeed even when the client loses its response. Cleanup then
        # resolves this exact name and still requires our random owner label.
        name = args[-1] if kind == 'network' else args[args.index('--name') + 1]
        require(name.startswith(self.prefix + '-'), 'RESOURCE_NAME_NOT_OWNED')
        self.created.append((kind, name))
        identity = self.docker([kind, 'create', '--label', OWNER_LABEL + '=' + self.owner, *args], env=env)
        require(identity and '\n' not in identity, 'RESOURCE_CREATE_ID_INVALID')
        self.created[-1] = (kind, identity)
        return identity

    def start(self):
        self.check_engine()
        self.check_images()
        network = self.create('network', ['--internal', self.prefix + '-net'])
        frontend = self.create('network', [self.prefix + '-frontend'])
        database_volume = self.create('volume', ['--name', self.prefix + '-db'])
        media_volume = self.create('volume', ['--name', self.prefix + '-media'])
        password = secrets.token_hex(32)
        env = os.environ.copy()
        env.update(POSTGRES_PASSWORD=password, DB_PASSWORD=password)
        common = ['--network', network, '--memory', '2g', '--cpus', '2']
        db = self.create('container', ['--name', self.prefix + '-db', '--network-alias', 'database', *common,
                          '--shm-size', '128m', '--mount', 'type=volume,source=' + database_volume + ',target=/var/lib/postgresql/data',
                          '--env', 'POSTGRES_PASSWORD', '--env', 'POSTGRES_USER=postgres',
                          '--env', 'POSTGRES_DB=immich', '--env', 'POSTGRES_INITDB_ARGS=--data-checksums', IMAGES['database']], env=env)
        redis = self.create('container', ['--name', self.prefix + '-redis', '--network-alias', 'redis', *common, IMAGES['redis']])
        for identity in (db, redis):
            self.docker(['container', 'start', identity])
        self.wait_for_backends(db, redis)

        server = self.create('container', ['--name', self.prefix + '-server', '--network', frontend, '--memory', '2g', '--cpus', '2',
                              '--publish', '127.0.0.1::2283', '--mount', 'type=volume,source=' + media_volume + ',target=/data',
                              '--env', 'DB_PASSWORD', '--env', 'DB_USERNAME=postgres', '--env', 'DB_DATABASE_NAME=immich',
                              '--env', 'DB_HOSTNAME=database', '--env', 'REDIS_HOSTNAME=redis', '--env', 'TZ=Etc/UTC',
                              '--env', 'CPU_CORES=2', '--env', 'IMMICH_LOG_LEVEL=error', IMAGES['server']], env=env)
        self.docker(['network', 'connect', network, server])
        self.docker(['container', 'start', server])
        info = json.loads(self.docker(['container', 'inspect', server]))[0]
        ports = info['NetworkSettings']['Ports']['2283/tcp']
        self.boundary = {'publishedPorts': info['NetworkSettings']['Ports'],
                         'requestedBindings': info['HostConfig']['PortBindings'],
                         'networkMode': info['HostConfig']['NetworkMode'],
                         'privileged': info['HostConfig'].get('Privileged'),
                         'mountTypes': [x['Type'] for x in info['Mounts']]}
        require(len(ports) == 1 and ports[0]['HostIp'] == '127.0.0.1', 'PUBLISHED_PORT_NOT_LOOPBACK')
        require(not info['HostConfig'].get('Privileged') and info['HostConfig']['NetworkMode'] == frontend,
                'CONTAINER_BOUNDARY_INVALID')
        require(all(x['Type'] == 'volume' for x in info['Mounts']), 'UNEXPECTED_HOST_MOUNT')
        self.origin = 'http://127.0.0.1:' + ports[0]['HostPort']
        deadline = time.monotonic() + 180
        while time.monotonic() < deadline:
            try:
                version = http_json(self.origin, '/server/version')
                check_version(version)
                return
            except Failure as error:
                if str(error) == 'SERVER_VERSION_MISMATCH':
                    raise
                time.sleep(1)
        self.capture_startup_diagnostics()
        raise Failure('SERVER_START_TIMEOUT')

    def wait_for_backends(self, database, cache):
        deadline = time.monotonic() + 60
        probes = (
            ('postgres', database, ['pg_isready', '-h', '127.0.0.1', '-U', 'postgres', '-d', 'immich', '-t', '1'], None),
            ('valkey', cache, ['valkey-cli', '-h', '127.0.0.1', 'ping'], 'PONG'),
        )
        last_states = {name: 'not_ready' for name, *_ in probes}
        while True:
            remaining = deadline - time.monotonic()
            if remaining <= 0:
                self.backend_diagnostics = [{'service': name, 'state': last_states[name]} for name, *_ in probes]
                self.capture_startup_diagnostics()
                raise Failure('BACKEND_START_TIMEOUT')

            all_ready = True
            for name, identity, command, expected in probes:
                remaining = deadline - time.monotonic()
                if remaining <= 0:
                    all_ready = False
                    break
                try:
                    output = self.docker(['container', 'exec', identity, *command], timeout=min(10, remaining))
                    ready = expected is None or output.strip() == expected
                except Failure:
                    ready = False
                last_states[name] = 'ready' if ready else 'not_ready'
                all_ready = all_ready and ready
            if all_ready:
                self.backend_diagnostics = [{'service': name, 'state': 'ready'} for name, *_ in probes]
                return

            remaining = deadline - time.monotonic()
            if remaining > 0:
                time.sleep(min(1, remaining))

    def capture_startup_diagnostics(self):
        # Never retain container configuration, environment, raw logs, or
        # healthcheck output: those may contain generated credentials.
        categories = {
            'dns': ('EAI_AGAIN', 'ENOTFOUND', 'could not translate host name'),
            'connection_refused': ('ECONNREFUSED', 'Connection refused'),
            'database_auth': ('password authentication failed',),
            'cpu_instruction': ('Illegal instruction', 'unsupported CPU'),
            'migration': ('Migration failed', 'migration failed'),
            'permission': ('Permission denied', 'EACCES'),
            'listening': ('Immich Server is listening',),
        }
        for kind, identity in self.created:
            if kind != 'container':
                continue
            try:
                rows = json.loads(self.docker(['container', 'inspect', identity]))
                require(len(rows) == 1 and rows[0].get('Config', {}).get('Labels', {}).get(OWNER_LABEL) == self.owner,
                        'RESOURCE_OWNER_CHANGED')
                state = rows[0].get('State', {})
                logs = self.docker(['container', 'logs', '--tail', '80', identity])
                self.startup_diagnostics.append({
                    'identity': identity,
                    'state': state.get('Status') if state.get('Status') in ('created', 'running', 'paused', 'restarting', 'removing', 'exited', 'dead') else 'unknown',
                    'exitCode': state.get('ExitCode') if type(state.get('ExitCode')) is int else None,
                    'oomKilled': state.get('OOMKilled') is True,
                    'health': state.get('Health', {}).get('Status') if state.get('Health', {}).get('Status') in ('starting', 'healthy', 'unhealthy') else 'not_available',
                    'logCategories': [name for name, needles in categories.items() if any(needle in logs for needle in needles)],
                })
            except (Failure, ValueError, TypeError, AttributeError):
                self.startup_diagnostics.append({'identity': identity, 'inspection': 'failed'})

    def cleanup(self):
        failures = []
        for kind, identity in reversed(self.created):
            try:
                rows = json.loads(self.docker([kind, 'inspect', identity]))
                require(len(rows) == 1, 'RESOURCE_INSPECTION_INVALID')
                item = rows[0]
                labels = item.get('Config', {}).get('Labels') if kind == 'container' else item.get('Labels')
                require(isinstance(labels, dict) and labels.get(OWNER_LABEL) == self.owner, 'RESOURCE_OWNER_CHANGED')
                args = [kind, 'rm']
                if kind == 'container':
                    args.append('--force')
                self.docker([*args, identity])
            except Failure as error:
                failures.append(str(error))
        if failures:
            raise Failure('OWNED_CLEANUP_FAILED:' + ','.join(failures))
        self.created.clear()


def bootstrap(server):
    password = secrets.token_hex(24)
    email = 'archivebridge-' + server.owner + '@example.invalid'
    http_json(server.origin, '/auth/admin-sign-up', 'POST', {'email': email, 'password': password, 'name': 'Synthetic ArchiveBridge integration'})
    login = http_json(server.origin, '/auth/login', 'POST', {'email': email, 'password': password})
    bearer = login['accessToken']
    # Keep ordinary API + microservice workers, including metadata extraction.
    # Disable the separate, unprovisioned ML service before uploading fixtures.
    config = http_json(server.origin, '/system-config', bearer=bearer)
    config['machineLearning']['enabled'] = False
    configured = http_json(server.origin, '/system-config', 'PUT', config, bearer=bearer)
    require(configured['machineLearning']['enabled'] is False, 'SYNTHETIC_ML_CONFIGURATION_FAILED')
    created = http_json(server.origin, '/api-keys', 'POST', {'name': 'Synthetic integration', 'permissions': PERMISSIONS}, bearer=bearer)
    key = created['secret']
    require(isinstance(key, str) and key, 'BOOTSTRAP_KEY_MISSING')
    own = http_json(server.origin, '/users/me', key=key)['id']
    require(own == login['userId'], 'BOOTSTRAP_ACCOUNT_MISMATCH')
    permissions = http_json(server.origin, '/api-keys/me', key=key)['permissions']
    require(set(permissions) == set(PERMISSIONS), 'BOOTSTRAP_SCOPE_MISMATCH')
    return key, bearer, own


class Native:
    def __init__(self, binary, output, secrets_to_check):
        self.binary = str(binary.resolve(strict=True))
        self.output = output
        self.commands = []
        self.secrets = secrets_to_check
        self.metadata_observations = []

    def run(self, args, key=None, success=True):
        env = os.environ.copy()
        if key:
            env[KEY_ENV] = key
        started = time.monotonic()
        try:
            process = subprocess.run([self.binary, *map(str, args)], cwd=self.output, env=env,
                                     capture_output=True, timeout=180, check=False)
        except (OSError, subprocess.TimeoutExpired):
            raise Failure('NATIVE_EXECUTION_FAILED') from None
        return self.record(args, process, time.monotonic() - started, success)

    def record(self, args, process, seconds, success):
        for secret in self.secrets:
            require(secret.encode() not in process.stdout + process.stderr, 'NATIVE_SECRET_LEAK')
        number = len(self.commands) + 1
        stdout = self.output / f'command-{number:02d}.stdout'
        stderr = self.output / f'command-{number:02d}.stderr'
        stdout.write_bytes(process.stdout)
        stderr.write_bytes(process.stderr)
        self.commands.append({'number': number, 'arguments': list(map(str, args)), 'exitCode': process.returncode,
                              'seconds': round(seconds, 6),
                              'stdoutSHA256': hashlib.sha256(process.stdout).hexdigest(),
                              'stderrSHA256': hashlib.sha256(process.stderr).hexdigest()})
        require((process.returncode == 0) == success, 'NATIVE_EXIT_UNEXPECTED_' + str(number))
        try:
            return json.loads(process.stdout)
        except ValueError:
            if not success:
                return None
            raise Failure('NATIVE_JSON_INVALID_' + str(number)) from None


class UploadResponseGate:
    """Bounded, fixture-only loopback proxy: hold one genuine upload response.

    This fault injector buffers at most 1 MiB and is never a media-performance
    measurement. It records method counts only; no credentials/headers/bodies.
    """
    def __init__(self, server_origin):
        target = urllib.parse.urlsplit(server_origin)
        require(target.scheme == 'http' and target.hostname == '127.0.0.1' and target.port,
                'FAULT_TARGET_INVALID')
        self.port = target.port
        self.accepted = threading.Event()
        self.release = threading.Event()
        self.uploads = 0
        self.error = None
        self.held_asset_id = None
        gate = self

        class Handler(http.server.BaseHTTPRequestHandler):
            protocol_version = 'HTTP/1.1'

            def log_message(self, *args):
                pass

            def body(self):
                maximum = 1024 * 1024
                if self.headers.get('Transfer-Encoding', '').lower() == 'chunked':
                    chunks, count = [], 0
                    while True:
                        line = self.rfile.readline(129)
                        require(len(line) <= 128 and line.endswith(b'\r\n'), 'FAULT_CHUNK_HEADER_INVALID')
                        try:
                            size = int(line.strip().split(b';')[0], 16)
                        except ValueError:
                            raise Failure('FAULT_CHUNK_HEADER_INVALID') from None
                        require(size >= 0 and count + size <= maximum, 'FAULT_BODY_TOO_LARGE')
                        if size == 0:
                            require(self.rfile.readline(129) == b'\r\n', 'FAULT_TRAILER_UNEXPECTED')
                            return b''.join(chunks)
                        data = self.rfile.read(size)
                        require(len(data) == size and self.rfile.read(2) == b'\r\n', 'FAULT_CHUNK_BODY_INVALID')
                        chunks.append(data)
                        count += size
                try:
                    size = int(self.headers.get('Content-Length', '0'))
                except ValueError:
                    raise Failure('FAULT_LENGTH_INVALID') from None
                require(0 <= size <= maximum, 'FAULT_BODY_TOO_LARGE')
                data = self.rfile.read(size)
                require(len(data) == size, 'FAULT_BODY_TRUNCATED')
                return data

            def dispatch(self):
                connection = None
                try:
                    require(self.client_address[0] == '127.0.0.1' and self.path.startswith('/api/'), 'FAULT_ROUTE_INVALID')
                    body = self.body()
                    headers = {name: self.headers[name] for name in ('x-api-key', 'Content-Type', 'Accept', 'Accept-Encoding')
                               if self.headers.get(name) is not None}
                    headers['Content-Length'] = str(len(body))
                    connection = http.client.HTTPConnection('127.0.0.1', gate.port, timeout=30)
                    connection.request(self.command, self.path, body=body, headers=headers)
                    response = connection.getresponse()
                    raw = response.read(8 * 1024 * 1024 + 1)
                    require(len(raw) <= 8 * 1024 * 1024, 'FAULT_RESPONSE_TOO_LARGE')
                    if self.command == 'POST' and self.path == '/api/assets':
                        gate.uploads += 1
                        if not gate.release.is_set():
                            require(response.status == 201, 'FAULT_UPLOAD_NOT_ACCEPTED')
                            uploaded = json.loads(raw)
                            require(uploaded.get('status') == 'created' and isinstance(uploaded.get('id'), str), 'FAULT_UPLOAD_NOT_NEW')
                            gate.held_asset_id = uploaded['id']
                            gate.accepted.set()
                            require(gate.release.wait(30), 'FAULT_RELEASE_TIMEOUT')
                    self.send_response(response.status)
                    self.send_header('Content-Type', response.getheader('Content-Type', 'application/json'))
                    self.send_header('Content-Length', str(len(raw)))
                    self.send_header('Connection', 'close')
                    self.end_headers()
                    self.wfile.write(raw)
                except (BrokenPipeError, ConnectionResetError):
                    pass  # Expected after killing the owned native process.
                except Exception as error:
                    gate.error = str(error) if isinstance(error, Failure) else 'FAULT_PROXY_ERROR'
                    try:
                        self.send_error(502, 'Synthetic fault proxy failed')
                    except OSError:
                        pass
                finally:
                    if connection:
                        connection.close()
                    self.close_connection = True

            do_GET = dispatch
            do_POST = dispatch
            do_PUT = dispatch
            do_DELETE = dispatch

        self.http = http.server.ThreadingHTTPServer(('127.0.0.1', 0), Handler)
        self.http.daemon_threads = True
        self.origin = 'http://127.0.0.1:' + str(self.http.server_port)
        self.thread = threading.Thread(target=self.http.serve_forever, daemon=True)

    def __enter__(self):
        self.thread.start()
        return self

    def __exit__(self, *args):
        self.release.set()
        self.http.shutdown()
        self.http.server_close()
        self.thread.join(3)


def load_journal(path, command, status):
    try:
        value = json.loads(path.read_text('utf-8'))
    except (OSError, ValueError):
        raise Failure('PRIVATE_JOURNAL_INVALID') from None
    require(value.get('schemaVersion') == 1 and value.get('product') == 'ArchiveBridge'
            and value.get('command') == command and value.get('status') == status
            and re.fullmatch('[0-9a-f]{32}', value.get('journalId', ''))
            and isinstance(value.get('report'), dict), 'PRIVATE_JOURNAL_INVALID')
    report = value['report']
    require(report.get('schemaVersion') == 1 and report.get('status') == status, 'PRIVATE_REPORT_INVALID')
    return report


def fixture(output, scene=0, directory='synthetic sources', folders=None):
    spec = importlib.util.spec_from_file_location('synthetic_archivebridge', ROOT / 'examples' / 'generate.py')
    generator = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(generator)
    target = output / directory
    target.mkdir()
    source = target / 'unicode two albums.zip'
    prefix = 'Takeout/Google Photos/'
    data = generator.png(scene)
    with zipfile.ZipFile(source, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
        for title in (folders or ['Aile İstanbul', 'Weekend photos']):
            name = 'göl manzarası.png'
            archive.writestr(prefix + title + '/' + name, data)
            archive.writestr(prefix + title + '/' + name + '.json', generator.metadata(name, 1700000000))
    return source


def remote_snapshot(server, key):
    assets = http_json(server.origin, '/search/metadata', 'POST', {'size': 1000}, key=key)['assets']
    require(assets['nextCursor'] is None, 'SYNTHETIC_INVENTORY_UNEXPECTED_PAGINATION')
    albums = http_json(server.origin, '/albums', key=key)
    relationships = {}
    for album in albums:
        members = http_json(server.origin, '/search/metadata', 'POST',
                            {'filter': {'albumIds': {'any': [album['id']]}}, 'size': 1000}, key=key)['assets']
        require(members['nextCursor'] is None, 'SYNTHETIC_ALBUM_UNEXPECTED_PAGINATION')
        relationships[album['id']] = sorted(x['id'] for x in members['items'])
    return {'assets': sorted(x['id'] for x in assets['items']), 'albums': relationships}


def conflicting_exif_fixture(output):
    """Keep source JSON date and conflicting embedded date independently fixed."""
    require(len(CONFLICTING_EXIF_JPEG) == 1764 and hashlib.sha256(CONFLICTING_EXIF_JPEG).hexdigest()
            == '26875c6fd8f73fffc3cc259086fa808ec22003ee20d5e69e9df1335a0d0ba058', 'EXIF_FIXTURE_BYTES_CHANGED')
    source = output / 'conflicting embedded date.zip'
    prefix = 'Takeout/Google Photos/Source date precedence/'
    metadata = {'title': 'edited.jpg', 'photoTakenTime': {'timestamp': '1700000000'}}
    with zipfile.ZipFile(source, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
        archive.writestr(prefix + 'edited.jpg', CONFLICTING_EXIF_JPEG)
        archive.writestr(prefix + 'edited.jpg.json', json.dumps(metadata).encode('utf-8'))
    return source


def verify_original(server, key, asset_id, expected_sha, expected_bytes):
    request = urllib.request.Request(server.origin + '/api/assets/' + asset_id + '/original',
                                     headers={'x-api-key': key, 'Accept-Encoding': 'identity'})
    opener = urllib.request.build_opener(urllib.request.ProxyHandler({}), NoRedirect())
    digest, count = hashlib.sha256(), 0
    try:
        with opener.open(request, timeout=20) as response:
            while True:
                chunk = response.read(1024 * 1024)
                if not chunk:
                    break
                count += len(chunk)
                require(count <= expected_bytes, 'REMOTE_ORIGINAL_TOO_LONG')
                digest.update(chunk)
    except (urllib.error.URLError, OSError, TimeoutError):
        raise Failure('REMOTE_ORIGINAL_READ_FAILED') from None
    require(count == expected_bytes and digest.hexdigest() == expected_sha, 'REMOTE_ORIGINAL_BYTES_WRONG')


def wait_processed_metadata(server, key, report):
    observations = []
    for content in report['contents']:
        asset_id = content.get('remoteAssetId')
        if not asset_id:
            require(content['state'] == 'skipped', 'PROCESSED_METADATA_ASSET_MISSING')
            continue
        dates = {item['date'] for item in report['files'] if item['sha256'] == content['sha256'] and item['state'] != 'skipped'}
        require(len(dates) == 1, 'SOURCE_DATE_NOT_UNIQUE')
        date = dates.pop()
        require(FIXTURE_DATES.get(content['sha256']) == date, 'REPORT_DATE_DIFFERS_FROM_KNOWN_FIXTURE')
        source_date = dt.datetime.fromisoformat(date.replace('Z', '+00:00'))
        deadline = time.monotonic() + 45
        while True:
            detail = http_json(server.origin, '/assets/' + asset_id, key=key)
            exif = detail.get('exifInfo') or {}
            if exif.get('exifImageWidth') == 320 and exif.get('exifImageHeight') == 200:
                actual_date = dt.datetime.fromisoformat(detail['fileCreatedAt'].replace('Z', '+00:00'))
                require(source_date.tzinfo is not None and actual_date.tzinfo is not None and actual_date == source_date,
                        'POST_PROCESSING_SOURCE_DATE_CHANGED')
                require(detail.get('hasMetadata') is True, 'METADATA_WORKER_NOT_OBSERVED')
                try:
                    exif_date = dt.datetime.fromisoformat(str(exif.get('dateTimeOriginal', '')).replace('Z', '+00:00'))
                except ValueError:
                    raise Failure('POST_PROCESSING_EXIF_DATE_CHANGED') from None
                require(exif_date.tzinfo is not None and exif_date == source_date, 'POST_PROCESSING_EXIF_DATE_CHANGED')
                observations.append({'assetId': asset_id, 'mediaSHA256': content['sha256'], 'expectedSourceDate': date,
                                     'fileCreatedAt': detail['fileCreatedAt'], 'exifImageWidth': exif['exifImageWidth'],
                                     'exifImageHeight': exif['exifImageHeight'], 'dateTimeOriginal': exif['dateTimeOriginal']})
                break
            require(time.monotonic() < deadline, 'METADATA_WORKER_NOT_OBSERVED')
            time.sleep(0.25)
    return observations


def skipped_sample_integration(native, server, key):
    spec = importlib.util.spec_from_file_location('synthetic_skip_sample', ROOT / 'examples' / 'generate.py')
    generator = importlib.util.module_from_spec(spec)
    spec.loader.exec_module(generator)
    sources = native.output / 'ambiguous sample sources'
    with contextlib.redirect_stdout(io.StringIO()):
        generator.create(sources)
    plan, archive = native.output / 'ambiguous-plan.json', native.output / 'ambiguous archive'
    native.run(['plan', '--source', sources / 'takeout-part-1.zip', '--source', sources / 'takeout-part-2.zip', '--output', plan, '--json'])
    native.run(['export', '--plan', plan, '--out', archive, '--json'])
    before = remote_snapshot(server, key)
    preview = native.run(['immich', 'plan', '--archive', archive, '--json'], success=False)
    require(preview['report']['status'] == 'blocked' and preview['report']['unresolvedCount'] == 1, 'AMBIGUOUS_PREVIEW_NOT_BLOCKED')
    common = ['--archive', archive, '--server', server.origin, '--api-key-env', KEY_ENV, '--allow-http-loopback', '--json']
    blocked = native.run(['immich', 'import', *common, '--report', native.output / 'ambiguous-blocked.json'], key=key, success=False)
    require(blocked['report']['status'] == 'blocked', 'AMBIGUOUS_IMPORT_NOT_BLOCKED')
    require(remote_snapshot(server, key) == before, 'AMBIGUOUS_DEFAULT_MUTATED_REMOTE')
    import_path = native.output / 'explicit-skip.json'
    native.run(['immich', 'import', *common, '--skip-unresolved', '--report', import_path], key=key)
    report = load_journal(import_path, 'immich.import', 'completed_with_skips')
    require(len(report['files']) == 6 and sum(f['state'] == 'skipped' for f in report['files']) == 1,
            'SKIPPED_OCCURRENCE_NOT_EXPLICIT')
    require(len(report['contents']) == 4 and sum(c['state'] == 'skipped' for c in report['contents']) == 1,
            'SKIPPED_CONTENT_NOT_EXPLICIT')
    require(len(report['albums']) == 3 and sum(a['state'] == 'skipped' for a in report['albums']) == 1,
            'SKIPPED_ALBUM_NOT_EXPLICIT')
    require(len(report['memberships']) == 5 and sum(m['state'] == 'skipped' for m in report['memberships']) == 1,
            'SKIPPED_RELATIONSHIP_NOT_EXPLICIT')
    require(len(report['sidecars']) == 7 and all(x['state'] == 'not_transferred' and x['transferred'] is False for x in report['sidecars']),
            'SIDECAR_TRANSFER_CLAIM_INCORRECT')
    after_import = remote_snapshot(server, key)
    native.metadata_observations.extend(wait_processed_metadata(server, key, report))
    # Verification must derive the exclusion scope from the prior explicit
    # import report; it has no --skip-unresolved command-line flag.
    verify_path = native.output / 'skip-verify.json'
    native.run(['immich', 'verify', *common, '--report', import_path, '--output', verify_path], key=key)
    checked = load_journal(verify_path, 'immich.verify', 'verified_with_skips')
    require(checked['verification']['status'] == 'verified_with_skips', 'SKIP_VERIFY_FALSE_COMPLETENESS')
    native.run(['immich', 'import', *common, '--skip-unresolved', '--resume', import_path,
                '--report', native.output / 'skip-resume.json'], key=key)
    require(remote_snapshot(server, key) == after_import, 'SKIP_RESUME_CREATED_EXTRA_RESOURCES')
    return ['ambiguous_default_blocked_before_writes', 'explicit_skips_and_excluded_relationships',
            'raw_sidecars_not_transferred', 'skip_aware_readonly_verification', 'skip_aware_resume']


def interrupted_import_integration(native, server, key):
    source = fixture(native.output, scene=3, directory='interrupt sources', folders=['Accepted response interruption'])
    plan, archive = native.output / 'interrupt-plan.json', native.output / 'interrupt archive'
    native.run(['plan', '--source', source, '--output', plan, '--json'])
    native.run(['export', '--plan', plan, '--out', archive, '--json'])
    before = remote_snapshot(server, key)
    interrupted_path = native.output / 'accepted-upload-interrupted.json'
    with UploadResponseGate(server.origin) as gate:
        common = ['--archive', archive, '--server', gate.origin, '--api-key-env', KEY_ENV, '--allow-http-loopback', '--json']
        args = ['immich', 'import', *common, '--report', interrupted_path]
        env = os.environ.copy()
        env[KEY_ENV] = key
        child = subprocess.Popen([native.binary, *map(str, args)], cwd=native.output,
                                 env=env, stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        started = time.monotonic()
        try:
            require(gate.accepted.wait(20), 'FAULT_ACCEPTED_UPLOAD_NOT_OBSERVED')
            require(gate.error is None and child.poll() is None, 'FAULT_CHILD_NOT_WAITING_FOR_RESPONSE')
            pending = load_journal(interrupted_path, 'immich.import', 'in_progress')
            require(len(pending['contents']) == 1 and pending['contents'][0]['state'] == 'in_flight'
                    and not pending['contents'][0].get('remoteAssetId'), 'DURABLE_PREFLIGHT_CHECKPOINT_MISSING')
            checkpoint = interrupted_path.read_bytes()
            # Only this Popen instance is terminated, after the real server has
            # accepted its file and while the native process awaits a response.
            child.kill()
            stdout, stderr = child.communicate(timeout=10)
            result = subprocess.CompletedProcess(child.args, child.returncode, stdout, stderr)
            native.record(args, result, time.monotonic() - started, success=False)
            native.commands[-1]['termination'] = 'forced-owned-child-after-server-accepted-upload'
            native.commands[-1]['pid'] = child.pid
            gate.release.set()
            accepted = remote_snapshot(server, key)
            require(set(accepted['assets']) == set(before['assets']) | {gate.held_asset_id}
                    and accepted['albums'] == before['albums'], 'INTERRUPTION_REMOTE_BOUNDARY_WRONG')
            resumed_path = native.output / 'accepted-upload-resumed.json'
            native.run(['immich', 'import', *common, '--resume', interrupted_path, '--report', resumed_path], key=key)
            resumed = load_journal(resumed_path, 'immich.import', 'completed')
            native.metadata_observations.extend(wait_processed_metadata(server, key, resumed))
            require(len(resumed['contents']) == 1 and resumed['contents'][0]['remoteAssetId'] == gate.held_asset_id
                    and resumed['contents'][0]['originalSha256Verified'], 'ACCEPTED_UPLOAD_NOT_RECONCILED')
            require(gate.uploads == 1 and gate.error is None, 'RESUME_BLINDLY_REPEATED_UPLOAD')
            require(interrupted_path.read_bytes() == checkpoint, 'RESUME_OVERWROTE_PRIOR_CHECKPOINT')
            final = remote_snapshot(server, key)
            require(set(final['assets']) == set(accepted['assets']) and len(final['albums']) == len(before['albums']) + 1,
                    'RESUME_CREATED_EXTRA_REMOTE_RESOURCES')
            verify_path = native.output / 'accepted-upload-verify.json'
            native.run(['immich', 'verify', *common, '--report', resumed_path, '--output', verify_path], key=key)
            load_journal(verify_path, 'immich.verify', 'verified')
            require(remote_snapshot(server, key) == final, 'RESUMED_VERIFICATION_MUTATED_RESOURCES')
        finally:
            gate.release.set()
            if child.poll() is None:
                child.kill()
                child.communicate(timeout=10)
    return ['accepted_upload_response_interruption', 'durable_in_flight_journal', 'explicit_resume_reconciles_full_sha256',
            'no_blind_upload_retry', 'prior_resume_report_preserved']


def integration(native, server, key, bearer, account):
    source = fixture(native.output)
    plan = native.output / 'local-plan.json'
    archive = native.output / 'portable archive'
    native.run(['plan', '--source', source, '--output', plan, '--json'])
    native.run(['export', '--plan', plan, '--out', archive, '--json'])
    preview = native.run(['immich', 'plan', '--archive', archive, '--json'])['report']
    require(preview['mediaOccurrences'] == 2 and preview['uniqueContents'] == 1
            and preview['sourceAlbums'] == 2 and preview['unresolvedCount'] == 0, 'OFFLINE_PLAN_COUNTS_WRONG')
    common = ['--archive', archive, '--server', server.origin, '--api-key-env', KEY_ENV, '--allow-http-loopback', '--json']
    report_path = native.output / 'import.json'
    native.run(['immich', 'import', *common, '--report', report_path], key=key)
    report = load_journal(report_path, 'immich.import', 'completed')
    require(report['status'] == 'completed' and report['accountId'] == account, 'IMPORT_NOT_COMPLETED')
    require(len(report['contents']) == 1 and report['contents'][0]['originalSha256Verified'], 'ORIGINAL_BYTES_NOT_VERIFIED')
    require(len(report['files']) == 2 and len(report['memberships']) == 2, 'OCCURRENCE_RELATIONSHIP_LOST')
    require(len(report['albums']) == 2, 'SOURCE_ALBUM_LOST')
    native.metadata_observations.extend(wait_processed_metadata(server, key, report))
    asset = report['contents'][0]['remoteAssetId']
    verify_original(server, key, asset, report['contents'][0]['sha256'], report['contents'][0]['bytes'])
    detail = http_json(server.origin, '/assets/' + asset, key=key)
    require(detail['ownerId'] == account, 'REMOTE_OWNER_MISMATCH')
    require(detail['originalFileName'] == 'göl manzarası.png', 'UNICODE_FILENAME_CHANGED')
    require(detail['fileCreatedAt'].startswith('2023-11-14T22:13:20'), 'REMOTE_DATE_CHANGED')
    album_ids = {a['remoteAlbumId'] for a in report['albums']}
    for album_id in album_ids:
        matches = http_json(server.origin, '/search/metadata', 'POST',
                            {'filter': {'albumIds': {'any': [album_id]}}, 'size': 1000}, key=key)
        require({a['id'] for a in matches['assets']['items']} == {asset}
                and matches['assets']['nextCursor'] is None, 'REMOTE_ALBUM_MEMBERSHIP_WRONG')
    verification_path = native.output / 'verify.json'
    before_verify = remote_snapshot(server, key)
    native.run(['immich', 'verify', *common, '--report', report_path, '--output', verification_path], key=key)
    checked = load_journal(verification_path, 'immich.verify', 'verified')
    require(checked['status'] == 'verified' and checked['verification']['status'] == 'verified', 'READONLY_VERIFY_FAILED')
    require(remote_snapshot(server, key) == before_verify, 'VERIFY_MUTATED_REMOTE_INVENTORY')
    repeated_path = native.output / 'repeat.json'
    native.run(['immich', 'import', *common, '--report', repeated_path], key=key)
    repeated = load_journal(repeated_path, 'immich.import', 'completed')
    require({a['remoteAlbumId'] for a in repeated['albums']} == album_ids
            and {a['remoteAssetId'] for a in repeated['contents']} == {asset}, 'REPEAT_CREATED_EXTRA_RESOURCES')
    require(remote_snapshot(server, key) == before_verify, 'REPEAT_CHANGED_REMOTE_INVENTORY')
    # Actual server creates a deliberately underprivileged key. The product must
    # reject it before a new upload or album mutation.
    denied_source = fixture(native.output, scene=1, directory='permission-test sources', folders=['Permission boundary'])
    denied_plan, denied_archive = native.output / 'permission-plan.json', native.output / 'permission archive'
    native.run(['plan', '--source', denied_source, '--output', denied_plan, '--json'])
    native.run(['export', '--plan', denied_plan, '--out', denied_archive, '--json'])
    # Upload and album creation remain granted. Only membership creation is
    # absent, so a late permission check could cause an observable mutation.
    limited = http_json(server.origin, '/api-keys', 'POST', {'name': 'Synthetic missing membership permission',
                        'permissions': [p for p in PERMISSIONS if p != 'albumAsset.create']}, bearer=bearer)['secret']
    native.secrets.append(limited)
    denied = native.run(['immich', 'import', '--archive', denied_archive, '--server', server.origin,
                        '--api-key-env', KEY_ENV, '--allow-http-loopback', '--json',
                        '--report', native.output / 'denied.json'], key=limited, success=False)
    require(denied.get('error', {}).get('code') == 'MISSING_PERMISSIONS'
            and denied.get('report', {}).get('status') == 'failed'
            and all(c.get('state') == 'pending' for c in denied['report']['contents']), 'MISSING_SCOPE_FAILURE_NOT_PREFLIGHT')
    after = http_json(server.origin, '/albums', key=key)
    require({a['id'] for a in after} == album_ids, 'PREFLIGHT_PERMISSION_CHANGED_ALBUMS')
    require(remote_snapshot(server, key) == before_verify, 'PREFLIGHT_PERMISSION_CHANGED_ASSETS')
    # Rotating a key on the same account must preserve resume identity.
    rotated = http_json(server.origin, '/api-keys', 'POST', {'name': 'Synthetic rotated key', 'permissions': PERMISSIONS}, bearer=bearer)['secret']
    native.secrets.append(rotated)
    native.run(['immich', 'import', *common, '--resume', report_path, '--report', native.output / 'rotated-resume.json'], key=rotated)
    require(remote_snapshot(server, key) == before_verify, 'ROTATED_KEY_RESUME_CREATED_RESOURCES')
    # A different user on the same server cannot reuse the prior journal.
    other_password = secrets.token_hex(24)
    other_email = 'foreign-' + server.owner + '@example.invalid'
    http_json(server.origin, '/admin/users', 'POST', {'email': other_email, 'password': other_password,
              'name': 'Synthetic separate account', 'notify': False, 'shouldChangePassword': False}, bearer=bearer)
    other_login = http_json(server.origin, '/auth/login', 'POST', {'email': other_email, 'password': other_password})
    other_bearer = other_login['accessToken']
    other_key = http_json(server.origin, '/api-keys', 'POST', {'name': 'Synthetic foreign account', 'permissions': PERMISSIONS}, bearer=other_bearer)['secret']
    native.secrets.extend([other_bearer, other_key, other_password])
    other_before = remote_snapshot(server, other_key)
    require(other_before == {'assets': [], 'albums': {}}, 'NEW_SYNTHETIC_ACCOUNT_NOT_EMPTY')
    foreign = native.run(['immich', 'import', *common, '--resume', report_path, '--report', native.output / 'foreign-resume.json'], key=other_key, success=False)
    require(foreign.get('error', {}).get('code') == 'RESUME_REPORT_INVALID', 'FOREIGN_RESUME_WRONG_FAILURE')
    require(remote_snapshot(server, other_key) == other_before and remote_snapshot(server, key) == before_verify,
            'FOREIGN_ACCOUNT_RESUME_CHANGED_RESOURCES')
    skip_checks = skipped_sample_integration(native, server, key)
    interrupt_checks = interrupted_import_integration(native, server, key)
    exif_checks = conflicting_exif_integration(native, server, key, account)
    for path in native.output.rglob('*'):
        if path.is_file() and path.suffix in ('.json', '.stdout', '.stderr'):
            raw = path.read_bytes()
            require(all(s.encode() not in raw for s in native.secrets), 'OUTPUT_SECRET_LEAK')
    return ['offline_unicode_preview', 'remote_original_sha256', 'source_date', 'unicode_filename',
            'duplicate_content_multi_album', 'readonly_remote_verification', 'repeat_no_extra_resources', 'seven_scope_preflight',
            'same_account_key_rotation_resume', 'different_account_resume_refused', 'source_dates_after_real_metadata_workers',
            *skip_checks, *interrupt_checks, *exif_checks]


def conflicting_exif_integration(native, server, key, account):
    source = conflicting_exif_fixture(native.output)
    plan, archive = native.output / 'exif-plan.json', native.output / 'exif archive'
    native.run(['plan', '--source', source, '--output', plan, '--json'])
    native.run(['export', '--plan', plan, '--out', archive, '--json'])
    before = remote_snapshot(server, key)
    common = ['--archive', archive, '--server', server.origin, '--api-key-env', KEY_ENV, '--allow-http-loopback', '--json']
    import_path = native.output / 'exif-import.json'
    native.run(['immich', 'import', *common, '--report', import_path], key=key)
    report = load_journal(import_path, 'immich.import', 'completed')
    require(report.get('dateTransferPolicy') == 'takeout-date-authoritative-generated-xmp-v1', 'DATE_TRANSFER_POLICY_UNDECLARED')
    require(len(report['contents']) == 1 and len(report['files']) == 1 and len(report['albums']) == 1
            and len(report['memberships']) == 1 and len(report['sidecars']) == 1, 'EXIF_CASE_COUNTS_WRONG')
    content, occurrence, sidecar = report['contents'][0], report['files'][0], report['sidecars'][0]
    require(content['sha256'] == '26875c6fd8f73fffc3cc259086fa808ec22003ee20d5e69e9df1335a0d0ba058'
            and content['bytes'] == 1764 and content['originalSha256Verified']
            and content['verifiedOriginalBytes'] == 1764 and occurrence['date'] == '2023-11-14T22:13:20Z', 'EXIF_ORIGINAL_OR_SOURCE_DATE_CHANGED')
    require(sidecar['state'] == 'not_transferred' and sidecar['transferred'] is False, 'RAW_TAKEOUT_JSON_WAS_TRANSFERRED')
    native.metadata_observations.extend(wait_processed_metadata(server, key, report))
    asset_id = content['remoteAssetId']
    verify_original(server, key, asset_id, content['sha256'], content['bytes'])
    detail = http_json(server.origin, '/assets/' + asset_id, key=key)
    require(detail['ownerId'] == account and detail['originalFileName'] == 'edited.jpg', 'EXIF_REMOTE_IDENTITY_CHANGED')
    after = remote_snapshot(server, key)
    require(set(after['assets']) == set(before['assets']) | {asset_id}
            and len(after['albums']) == len(before['albums']) + 1, 'EXIF_IMPORT_INVENTORY_WRONG')
    native.run(['immich', 'verify', *common, '--report', import_path, '--output', native.output / 'exif-verify.json'], key=key)
    load_journal(native.output / 'exif-verify.json', 'immich.verify', 'verified')
    native.run(['immich', 'import', *common, '--report', native.output / 'exif-repeat.json'], key=key)
    repeated = load_journal(native.output / 'exif-repeat.json', 'immich.import', 'completed')
    require(repeated['contents'][0]['remoteAssetId'] == asset_id and remote_snapshot(server, key) == after,
            'EXIF_VERIFY_OR_REPEAT_MUTATED_INVENTORY')
    return ['takeout_date_survives_conflicting_embedded_exif_after_workers']


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', required=True, type=Path, help='Fresh private output directory')
    parser.add_argument('--binary', type=Path)
    parser.add_argument('--expected-version', default='1.0.0')
    parser.add_argument('--expected-commit', default='development')
    parser.add_argument('--bootstrap-only', action='store_true', help='Server infrastructure check only; not product qualification')
    args = parser.parse_args()
    require(args.bootstrap_only or args.binary, 'BINARY_REQUIRED')
    require(args.expected_commit == 'development' or re.fullmatch('[0-9a-f]{40}', args.expected_commit), 'EXPECTED_COMMIT_INVALID')
    args.out = args.out.resolve()
    require(args.out.parent.is_dir() and not args.out.exists(), 'FRESH_OUTPUT_REQUIRED')
    args.out.mkdir()
    server = OwnedServer()
    report = {'schemaVersion': 1, 'status': 'failed', 'qualification': False,
              'scope': 'infrastructure-only' if args.bootstrap_only else 'native-real-immich-synthetic',
              'platform': platform.system(), 'synthetic': True, 'serverVersion': '3.3.1', 'images': IMAGES,
              'checks': [], 'cleanup': 'not_started'}
    script_bytes = Path(__file__).read_bytes()
    report['harness'] = {'name': Path(__file__).name, 'bytes': len(script_bytes), 'sha256': hashlib.sha256(script_bytes).hexdigest()}
    report['workers'] = {'api': True, 'microservices': True, 'machineLearning': False}
    started = time.monotonic()
    error = None
    try:
        server.start()
        key, bearer, account = bootstrap(server)
        report['checks'] = ['pinned_images', 'loopback_only', 'exact_server_version', 'synthetic_account', 'seven_scoped_key']
        if not args.bootstrap_only:
            native = Native(args.binary, args.out, [key, bearer])
            report['binary'] = {'bytes': args.binary.stat().st_size, 'sha256': hashlib.sha256(args.binary.read_bytes()).hexdigest()}
            try:
                identity = native.run(['version', '--json'])
                require(identity.get('status') == 'ok' and identity.get('schemaVersion') == 1
                        and identity.get('version') == args.expected_version and identity.get('commit') == args.expected_commit,
                        'NATIVE_IDENTITY_MISMATCH')
                report['binary']['version'] = identity['version']
                report['binary']['commit'] = identity['commit']
                report['checks'].extend(integration(native, server, key, bearer, account))
            finally:
                report['commands'] = native.commands
                report['metadataObservations'] = native.metadata_observations
        report['status'] = 'passed'
    except Failure as failure:
        error = str(failure)
    except Exception:
        # Unexpected programming errors are not rendered with raw exception
        # values, which can contain HTTP responses or account credentials.
        error = 'HARNESS_UNEXPECTED_EXCEPTION'
    finally:
        report['ownedResources'] = [{'kind': kind, 'identity': identity} for kind, identity in server.created]
        report['ownerLabel'] = {OWNER_LABEL: server.owner}
        try:
            server.cleanup()
            report['cleanup'] = 'passed'
        except Failure as failure:
            report['cleanup'] = 'failed'
            error = str(failure)
        report['seconds'] = round(time.monotonic() - started, 6)
        report['imageInventory'] = server.inventory
        report['engine'] = server.engine
        report['boundary'] = server.boundary
        if server.startup_diagnostics:
            report['startupDiagnostics'] = server.startup_diagnostics
        if server.backend_diagnostics:
            report['backendDiagnostics'] = server.backend_diagnostics
        if error:
            report['status'] = 'failed'
            report['error'] = error
        (args.out / 'integration-report.json').write_text(json.dumps(report, indent=2) + '\n', encoding='utf-8')
    print(json.dumps({'status': report['status'], 'scope': report['scope'], 'cleanup': report['cleanup'], 'report': str(args.out / 'integration-report.json')}))
    return 0 if report['status'] == 'passed' else 1


def validate_local_endpoint(endpoint):
    require(isinstance(endpoint, str), 'LOCAL_DOCKER_ENDPOINT_REQUIRED')
    ordinary_unix = endpoint in ('unix:///var/run/docker.sock', 'unix:///run/docker.sock')
    rootless_unix = re.fullmatch(r'unix:///run/user/[0-9]+/docker\.sock', endpoint) is not None
    desktop_pipe = endpoint.lower() in ('npipe:////./pipe/dockerdesktoplinuxengine', 'npipe:////./pipe/docker_engine')
    require(ordinary_unix or rootless_unix or desktop_pipe, 'LOCAL_DOCKER_ENDPOINT_REQUIRED')


if __name__ == '__main__':
    try:
        raise SystemExit(main())
    except Failure as failure:
        raise SystemExit(str(failure)) from None
