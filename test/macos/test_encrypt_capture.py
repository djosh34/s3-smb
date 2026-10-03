# SPDX-License-Identifier: AGPL-3.0-only
"""Tiny synthetic archive tests; never use the real capture recipient private key."""
import json
import os
from pathlib import Path
import shutil
import subprocess
import tarfile
import tempfile
import unittest

SCRIPT = Path(__file__).with_name('encrypt_capture.sh').resolve()


@unittest.skipUnless(shutil.which('openssl'), 'OpenSSL unavailable')
class EncryptCaptureTest(unittest.TestCase):
    @classmethod
    def setUpClass(cls):
        if not subprocess.check_output(['openssl', 'version']).startswith(b'OpenSSL 3.'):
            raise unittest.SkipTest('reviewed scheme needs OpenSSL 3')
        cls.root = tempfile.TemporaryDirectory()
        cls.key = Path(cls.root.name) / 'synthetic-key.pem'
        cls.cert = Path(cls.root.name) / 'synthetic-cert.pem'
        subprocess.run(['openssl', 'req', '-x509', '-newkey', 'rsa:2048', '-nodes',
                        '-keyout', cls.key, '-out', cls.cert, '-subj', '/CN=synthetic-test-only',
                        '-days', '1'], stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL, check=True)

    @classmethod
    def tearDownClass(cls):
        cls.root.cleanup()

    def setUp(self):
        self.old_umask = os.umask(0o077)
        self.addCleanup(os.umask, self.old_umask)

    def retained(self, root, raw_present):
        raw, logs, public = root / 'private-traffic.pcap', root / 'private-logs', root / 'public'
        logs.mkdir(mode=0o700)
        public.mkdir(mode=0o700)
        (logs / 'synthetic.log').write_text('synthetic private log only')
        if raw_present:
            raw.write_bytes(b'synthetic-not-real-pcap')
        result = subprocess.run(['bash', str(SCRIPT), str(raw), str(public), str(self.cert), str(logs)],
            env={**os.environ, 'CAPTURE_OPENSSL': shutil.which('openssl')},
            stdout=subprocess.PIPE, stderr=subprocess.PIPE)
        self.assertEqual(result.returncode, 0)  # Never print command stderr/private data.
        return public

    def decrypt(self, cipher, output):
        return subprocess.run(['openssl', 'cms', '-decrypt', '-binary', '-inform', 'DER',
            '-in', str(cipher), '-recip', str(self.cert), '-inkey', str(self.key), '-out', str(output)],
            stdout=subprocess.DEVNULL, stderr=subprocess.DEVNULL).returncode

    def test_both_capture_and_private_logs_retained(self):
        with tempfile.TemporaryDirectory() as path:
            root = Path(path)
            public = self.retained(root, True)
            metadata = json.loads((public / 'capture-encryption.json').read_text())
            self.assertTrue(metadata['raw_capture_present'])
            self.assertFalse(metadata['plaintext_uploaded'])
            output = root / 'authenticated.tar.gz'
            self.assertEqual(self.decrypt(public / 'capture.tar.gz.cms', output), 0)
            with tarfile.open(output, 'r:gz') as archive:
                self.assertEqual(set(archive.getnames()),
                                 {'private-traffic.pcap', 'private-logs', 'private-logs/synthetic.log'})
            self.assertFalse(any('key' in p.name for p in public.iterdir()))

    def test_no_capture_retains_logs_without_inventing_capture(self):
        with tempfile.TemporaryDirectory() as path:
            root = Path(path)
            public = self.retained(root, False)
            metadata = json.loads((public / 'capture-encryption.json').read_text())
            self.assertFalse(metadata['raw_capture_present'])
            self.assertIsNone(metadata['raw_bytes'])
            output = root / 'authenticated.tar.gz'
            self.assertEqual(self.decrypt(public / 'capture.tar.gz.cms', output), 0)
            with tarfile.open(output, 'r:gz') as archive:
                self.assertEqual(set(archive.getnames()), {'private-logs', 'private-logs/synthetic.log'})

    def test_truncated_ciphertext_rejected_before_archive_parse(self):
        with tempfile.TemporaryDirectory() as path:
            root = Path(path)
            public = self.retained(root, True)
            data = (public / 'capture.tar.gz.cms').read_bytes()
            truncated = root / 'truncated.cms'
            truncated.write_bytes(data[:-12])
            self.assertNotEqual(self.decrypt(truncated, root / 'unauthenticated-partial'), 0)
            # Deliberately do not parse unauthenticated partial plaintext.


if __name__ == '__main__':
    unittest.main()
