import contextlib
import io
import json
import os
from pathlib import Path
import stat
import tempfile
import unittest
from unittest.mock import patch

from codex_gateway import cli, config


class ConfigurationWorkflowTests(unittest.TestCase):
    def setUp(self):
        self.temp = tempfile.TemporaryDirectory()
        self.addCleanup(self.temp.cleanup)
        self.root = Path(self.temp.name)
        self.home = self.root / 'gateway'
        self.codex = self.root / 'codex'; self.codex.mkdir()
        self.saved_config = b'model="existing"\n[history]\npersistence="save-all"\n'
        (self.codex / 'config.toml').write_bytes(self.saved_config)
        self.template = {'slug': 'template-model', 'display_name': 'Template model',
                         'context_window': 32000, 'input_modalities': ['text'],
                         'supported_reasoning_levels': [{'effort': 'medium', 'description': 'Medium'}],
                         'upgrade': {'id': 'do-not-route'}, 'use_responses_lite': True,
                         'model_messages': {'custom': 'preserved model metadata'}}
        self.catalog = self.root / 'catalog.json'
        self.catalog.write_text(json.dumps({'models': [self.template]}))

    def command(self, *arguments, expected=0):
        out, err = io.StringIO(), io.StringIO()
        with contextlib.redirect_stdout(out), contextlib.redirect_stderr(err):
            result = cli.main(['--home', str(self.home), *arguments])
        self.assertEqual(result, expected, err.getvalue())
        return out.getvalue(), err.getvalue()

    def initialize(self, auth='token'):
        self.command('init', '--auth-mode', auth, '--codex-home', str(self.codex), '--catalog', str(self.catalog))

    def add_provider(self):
        self.command('provider', 'add', 'relay', '--base-url', 'https://api.example.com/v1',
                     '--api-key-env', 'EXAMPLE_API_KEY')

    def add_model(self):
        self.command('model', 'add', 'relay/model', '--provider', 'relay',
                     '--upstream-model', 'namespace/upstream-model', '--template', 'template-model')

    def test_full_configuration_keeps_codex_and_credentials_separate(self):
        self.initialize(); self.add_provider(); self.add_model()
        data = config.load_config(self.home)
        self.assertEqual(data['models']['relay/model']['model'], 'namespace/upstream-model')
        self.assertEqual(data['default_model'], 'relay/model')
        model = config.load_catalog(self.home)['models'][0]
        self.assertEqual(model['slug'], 'relay/model')
        self.assertEqual(model['model_messages'], self.template['model_messages'])
        self.assertFalse(model['use_responses_lite'])
        self.assertEqual(model['context_window'], 32000)
        self.assertIsNone(model['upgrade'])
        self.assertEqual(json.loads(self.catalog.read_text())['models'][0], self.template)
        self.assertEqual((self.codex / 'config.toml').read_bytes(), self.saved_config)
        self.assertNotEqual(config.read_token(self.home), config.read_token(self.home, 'client'))
        for path in self.home.iterdir():
            self.assertEqual(stat.S_IMODE(path.stat().st_mode), 0o600)
        self.assertEqual(stat.S_IMODE(self.home.stat().st_mode), 0o700)

    def test_init_refuses_to_overwrite_existing_data(self):
        self.initialize()
        before = {p.name: p.read_bytes() for p in self.home.iterdir()}
        self.command('init', expected=1)
        self.assertEqual({p.name: p.read_bytes() for p in self.home.iterdir()}, before)

    def test_missing_template_does_not_partially_update_configuration(self):
        self.initialize(); self.add_provider()
        before = (self.home / 'config.json').read_bytes()
        self.command('model', 'add', 'bad', '--provider', 'relay', '--upstream-model', 'model',
                     '--template', 'nonexistent', expected=1)
        self.assertEqual((self.home / 'config.json').read_bytes(), before)
        self.assertEqual(config.load_catalog(self.home)['models'], [])

    def test_invalid_provider_urls_are_rejected_before_write(self):
        self.initialize()
        before = (self.home / 'config.json').read_bytes()
        for url in ['http://remote.example.com/v1', 'https://user:secret@example.com/v1',
                    'https://example.com/v1?key=secret', 'file:///tmp/key', 'https://example.com/#fragment']:
            with self.subTest(url=url):
                self.command('provider', 'add', 'bad', '--base-url', url, '--api-key-env', 'KEY', expected=1)
                self.assertEqual((self.home / 'config.json').read_bytes(), before)
        self.command('provider', 'add', 'trusted', '--base-url', 'http://internal.example.com/v1',
                     '--api-key-env', 'KEY', '--allow-insecure-http')
        self.command('provider', 'add', 'local', '--base-url', 'http://127.0.0.1:9000/v1', '--api-key-env', 'KEY')

    def test_codex_passthrough_is_not_allowed_in_local_token_mode(self):
        self.initialize()
        self.command('provider', 'add', 'official', '--auth', 'codex',
                     '--base-url', 'https://chatgpt.com/backend-api/codex', expected=1)
        self.command('config', 'set', 'client_auth', 'codex')
        self.command('provider', 'add', 'official', '--auth', 'codex',
                     '--base-url', 'https://chatgpt.com/backend-api/codex')
        self.command('config', 'set', 'client_auth', 'token', expected=1)

    def test_dependency_removal_and_replacement_are_explicit(self):
        self.initialize(); self.add_provider(); self.add_model()
        self.command('provider', 'remove', 'relay', expected=1)
        self.command('provider', 'add', 'relay', '--base-url', 'https://new.example.com/v1',
                     '--api-key-env', 'NEW_KEY', expected=1)
        self.command('provider', 'add', 'relay', '--base-url', 'https://new.example.com/v1',
                     '--api-key-env', 'NEW_KEY', '--replace')
        self.command('model', 'remove', 'relay/model')
        self.command('provider', 'remove', 'relay')
        self.assertEqual(config.load_config(self.home)['providers'], {})

    def test_keys_are_references_and_never_printed(self):
        self.initialize(); self.add_provider()
        with patch.dict(os.environ, {'EXAMPLE_API_KEY': 'private-dummy-key'}):
            self.command('validate', '--credentials')
            out, _ = self.command('config', 'show')
            self.assertNotIn('private-dummy-key', out)
            self.assertEqual(config.resolve_api_key(config.load_config(self.home)['providers']['relay']), 'private-dummy-key')
        with patch.dict(os.environ, {}, clear=True):
            _, err = self.command('validate', '--credentials', expected=1)
            self.assertIn('EXAMPLE_API_KEY', err)
        keyfile = self.root / 'private-key'; keyfile.write_text('file-dummy-key\n')
        self.command('provider', 'add', 'files', '--base-url', 'https://example.com/v1', '--api-key-file', str(keyfile))
        provider = config.load_config(self.home)['providers']['files']
        self.assertEqual(config.resolve_api_key(provider), 'file-dummy-key')
        self.assertNotIn('file-dummy-key', (self.home / 'config.json').read_text())

    def test_catalog_import_merges_and_refreshes_existing_models(self):
        self.initialize(); self.add_provider(); self.add_model()
        other = self.root / 'other.json'
        other.write_text(json.dumps({'models': [{**self.template, 'context_window': 64000}, {'slug': 'other'}]}))
        self.command('catalog', 'import', str(other))
        self.assertEqual(config.load_catalog(self.home)['models'][0]['context_window'], 64000)
        self.assertEqual(len(config.templates_from(self.home / 'templates.json')['models']), 2)

    def test_empty_cache_can_be_initialized_and_imported_later(self):
        self.command('init', '--auth-mode', 'token', '--codex-home', str(self.codex))
        self.assertEqual(config.templates_from(self.home / 'templates.json'), {'models': []})
        self.command('catalog', 'import', str(self.catalog))
        self.add_provider(); self.add_model()

    def test_invalid_settings_and_unsupported_plaintext_keys_are_rejected(self):
        self.initialize()
        for key, value in [('listen.port', 'true'), ('listen.port', '0'), ('codex_home', 'null'),
                           ('default_model', 'missing'), ('retry_invalid_encrypted_reasoning', '1')]:
            with self.subTest(key=key, value=value):
                self.command('config', 'set', key, value, expected=1)
        data = config.load_config(self.home)
        data['secrets'] = {'key': 'do-not-store'}
        with self.assertRaises(config.GatewayError):
            config.save_config(self.home, data)


if __name__ == '__main__':
    unittest.main()
