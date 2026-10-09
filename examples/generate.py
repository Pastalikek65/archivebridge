#!/usr/bin/env python3
"""Create two small, deterministic Takeout-shaped archives from synthetic data."""
import argparse
import binascii
import hashlib
import json
import pathlib
import struct
import zipfile
import zlib


def png(scene):
    width, height = 320, 200
    rows = bytearray()
    palettes = [(165, 195, 190), (172, 186, 145), (130, 172, 185), (185, 163, 137)]
    red, green, blue = palettes[scene]
    for y in range(height):
        rows.append(0)
        for x in range(width):
            if y < 105:
                color = (min(255, red + y // 4), min(255, green + y // 5), min(255, blue + y // 5))
            elif y < 130 + ((x * (scene + 2)) % 43):
                color = (74 + scene * 6, 112 + scene * 4, 93)
            else:
                color = (112 + scene * 9, 143 + scene * 4, 121 + scene * 7)
            if 225 < x < 256 and 30 < y < 61:
                color = (238, 222, 174)
            rows.extend(color)
    def chunk(kind, data):
        return struct.pack('>I', len(data)) + kind + data + struct.pack('>I', binascii.crc32(kind + data) & 0xffffffff)
    return b'\x89PNG\r\n\x1a\n' + chunk(b'IHDR', struct.pack('>IIBBBBB', width, height, 8, 2, 0, 0, 0)) + chunk(b'IDAT', zlib.compress(bytes(rows), 9)) + chunk(b'IEND', b'')


def metadata(title, timestamp):
    return (json.dumps({'title': title, 'description': 'Synthetic demonstration only.', 'photoTakenTime': {'timestamp': str(timestamp)}}, sort_keys=True) + '\n').encode('utf-8')


def create(destination):
    destination.mkdir(parents=False, exist_ok=False)
    prefix = 'Takeout/Google Photos/'
    harbor, trail, lake, collision = [png(scene) for scene in range(4)]
    parts = [
        {
            prefix + 'Photos from 2023/harbor.png': harbor,
            prefix + 'Photos from 2023/harbor.png.json': metadata('harbor.png', 1700000000),
            prefix + 'Weekend/harbor.png': harbor,
            prefix + 'Weekend/harbor.png.json': metadata('harbor.png', 1700000000),
            prefix + 'Weekend/trail.png': trail,
            prefix + 'Weekend/trail.png.json': metadata('trail.png', 1700086400),
        },
        {
            prefix + 'Family/harbor.png': harbor,
            prefix + 'Family/harbor.png.json': metadata('harbor.png', 1700000000),
            prefix + 'Family/lake.png': lake,
            prefix + 'Family/lake.png.json': metadata('lake.png', 1699999999),
            prefix + 'Ambiguous/collision.png': collision,
            prefix + 'Ambiguous/first.json': metadata('collision.png', 1700000000),
            prefix + 'Ambiguous/second.json': metadata('collision.png', 1700086400),
            prefix + 'README.txt': b'This unsupported member is a synthetic omission example.\n',
        },
    ]
    identities = []
    for number, members in enumerate(parts, 1):
        output = destination / f'takeout-part-{number}.zip'
        with zipfile.ZipFile(output, 'x', compression=zipfile.ZIP_DEFLATED) as archive:
            for name, data in sorted(members.items()):
                entry = zipfile.ZipInfo(name, (2024, 1, 1, 0, 0, 0))
                entry.create_system = 3
                entry.external_attr = 0o100644 << 16
                entry.compress_type = zipfile.ZIP_DEFLATED
                archive.writestr(entry, data)
        raw = output.read_bytes()
        identities.append({'name': output.name, 'bytes': len(raw), 'sha256': hashlib.sha256(raw).hexdigest()})
    expected = {'schemaVersion': 1, 'synthetic': True, 'mediaOccurrences': 6, 'uniqueMediaContent': 4, 'sidecarOccurrences': 7, 'albums': ['Ambiguous', 'Family', 'Weekend'], 'ambiguousFile': prefix + 'Ambiguous/collision.png', 'sourceArchives': identities}
    with (destination / 'expected.json').open('x', encoding='utf-8', newline='\n') as file:
        json.dump(expected, file, indent=2)
        file.write('\n')
    print(json.dumps({'status': 'created-synthetic-sample', 'directory': str(destination), 'sources': identities}))


if __name__ == '__main__':
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument('--out', required=True, type=pathlib.Path, help='Fresh directory under an existing parent; never overwritten.')
    arguments = parser.parse_args()
    try:
        create(arguments.out)
    except (OSError, ValueError) as error:
        raise SystemExit(f'Sample generation failed: {error}')
