"""Portable configuration, credential references and Codex model catalogs."""

import copy
import hashlib
import ipaddress
import json
import os
from pathlib import Path
import re
import secrets
import tempfile
import tomllib
from urllib.parse import urlsplit

from . import __version__


class GatewayError(Exception):
    """An actionable configuration or gateway lifecycle error."""


def default_home():
    explicit = os.environ.get('CODEX_GATEWAY_HOME')
    base = Path(os.environ.get('XDG_CONFIG_HOME', str(Path.home() / '.config')))
    return Path(explicit).expanduser().resolve() if explicit else (base / 'codex-gateway').resolve()


def read_json(path):
    try:
        return json.loads(Path(path).read_text())
    except FileNotFoundError:
        raise GatewayError(f'Missing {path}; run codex-gateway init first') from None
    except (ValueError, UnicodeError):
        raise GatewayError(f'Invalid JSON in {path}') from None


def private_write(path, text):
    """Replace one file atomically without exposing an intermediate public file."""
    path = Path(path)
    path.parent.mkdir(parents=True, exist_ok=True, mode=0o700)
    temporary = None
    try:
        with tempfile.NamedTemporaryFile(mode='w', encoding='utf-8', dir=path.parent, delete=False) as out:
            temporary = Path(out.name)
            out.write(text)
            out.flush()
            os.fsync(out.fileno())
        temporary.replace(path)
    finally:
        if temporary is not None:
            temporary.unlink(missing_ok=True)


def write_json(path, data):
    private_write(path, json.dumps(data, ensure_ascii=False, indent=2) + '\n')


def read_token(home, name='admin'):
    if name not in ('admin', 'client'):
        raise GatewayError('Unknown local token type')
    try:
        token = (Path(home) / (name + '-token')).read_text().strip()
    except OSError:
        raise GatewayError(f'Missing local {name} token; initialize a gateway home') from None
    if len(token) < 24 or any(c.isspace() for c in token):
        raise GatewayError(f'Invalid local {name} token')
    return token


def loopback(host):
    if host == 'localhost':
        return True
    try:
        return ipaddress.ip_address(host).is_loopback
    except ValueError:
        return False


def validate_url(value, label, allow_http=False, proxy=False):
    if not isinstance(value, str) or not value or any(c.isspace() for c in value):
        raise GatewayError(f'{label} must be a URL without whitespace')
    try:
        url = urlsplit(value)
        port = url.port
    except ValueError:
        raise GatewayError(f'Invalid {label}') from None
    if (url.scheme not in ('http', 'https') or not url.hostname or url.username is not None
            or url.password is not None or url.query or url.fragment
            or (port is not None and not 1 <= port <= 65535)):
        raise GatewayError(f'{label} requires HTTP(S), without credentials, query or fragment')
    if proxy and url.path not in ('', '/'):
        raise GatewayError(f'{label} cannot include a path')
    if not proxy and url.scheme == 'http' and not loopback(url.hostname) and not allow_http:
        raise GatewayError(f'{label} uses remote HTTP; explicitly allow insecure HTTP for a trusted network')


def validate_config(config):
    if not isinstance(config, dict) or type(config.get('version')) is not int or config.get('version') != 1:
        raise GatewayError('Unsupported config version; expected version 1')
    allowed = {'version', 'listen', 'codex_home', 'client_auth', 'default_model', 'bootstrap_proxy',
               'retry_invalid_encrypted_reasoning', 'providers', 'models'}
    if set(config) - allowed:
        raise GatewayError('Unsupported configuration fields; credentials belong in environment variables or private files')
    listen = config.get('listen', {})
    if not isinstance(listen, dict) or listen.get('host') != '127.0.0.1':
        raise GatewayError('listen.host must be 127.0.0.1; this gateway is local only')
    port = listen.get('port')
    if type(port) is not int or not 1 <= port <= 65535:
        raise GatewayError('listen.port must be between 1 and 65535')
    if config.get('client_auth') not in ('codex', 'token'):
        raise GatewayError('client_auth must be codex or token')
    codex_home = config.get('codex_home')
    if not isinstance(codex_home, str) or not Path(codex_home).is_absolute():
        raise GatewayError('codex_home must be an absolute path')
    if type(config.get('retry_invalid_encrypted_reasoning', False)) is not bool:
        raise GatewayError('retry_invalid_encrypted_reasoning must be boolean')
    if config.get('bootstrap_proxy'):
        validate_url(config['bootstrap_proxy'], 'bootstrap_proxy', proxy=True)
    providers, models = config.get('providers'), config.get('models')
    if not isinstance(providers, dict) or not isinstance(models, dict):
        raise GatewayError('providers and models must be objects')
    for name, provider in providers.items():
        if not re.fullmatch(r'[A-Za-z][A-Za-z0-9_-]{0,63}', name) or not isinstance(provider, dict):
            raise GatewayError('Provider names must be letters, digits, underscores or hyphens')
        allowed = {'base_url', 'auth', 'api_key_env', 'api_key_file', 'proxy', 'allow_insecure_http'}
        if set(provider) - allowed:
            raise GatewayError(f'Provider {name} has unsupported fields; keep credentials in env vars or files')
        insecure = provider.get('allow_insecure_http', False)
        if type(insecure) is not bool:
            raise GatewayError(f'Provider {name}: allow_insecure_http must be boolean')
        validate_url(provider.get('base_url'), f'Provider {name} base_url', allow_http=insecure)
        if provider.get('proxy'):
            validate_url(provider['proxy'], f'Provider {name} proxy', proxy=True)
        if provider.get('auth') == 'codex':
            if config['client_auth'] != 'codex':
                raise GatewayError('ChatGPT passthrough providers require client_auth=codex')
            if provider.get('api_key_env') or provider.get('api_key_file'):
                raise GatewayError(f'Provider {name}: codex auth cannot contain an API key reference')
        elif provider.get('auth') == 'api_key':
            env, file = provider.get('api_key_env'), provider.get('api_key_file')
            if bool(env) == bool(file):
                raise GatewayError(f'Provider {name}: specify exactly one api_key_env or api_key_file')
            if env and (not isinstance(env, str) or not re.fullmatch(r'[A-Za-z_][A-Za-z0-9_]*', env)):
                raise GatewayError(f'Provider {name}: invalid API key environment variable name')
            if file and (not isinstance(file, str) or not Path(file).is_absolute()):
                raise GatewayError(f'Provider {name}: api_key_file must be an absolute path')
        else:
            raise GatewayError(f'Provider {name}: auth must be codex or api_key')
    for alias, model in models.items():
        if (not isinstance(alias, str) or not re.fullmatch(r'[A-Za-z0-9][A-Za-z0-9_./:-]{0,255}', alias)
                or not isinstance(model, dict)):
            raise GatewayError('Invalid model alias')
        if set(model) - {'provider', 'model', 'template', 'display_name'}:
            raise GatewayError(f'Model {alias} has unsupported fields')
        if model.get('provider') not in providers:
            raise GatewayError(f'Model {alias} references an unknown provider')
        for key in ('model', 'template'):
            value = model.get(key)
            if not isinstance(value, str) or not value or len(value) > 512 or any(c.isspace() for c in value):
                raise GatewayError(f'Model {alias}: {key} must be a nonempty identifier')
        if model.get('display_name') is not None and not isinstance(model['display_name'], str):
            raise GatewayError(f'Model {alias}: display_name must be a string')
    if config.get('default_model') is not None and config['default_model'] not in models:
        raise GatewayError('default_model must name a configured model alias')
    return config


def load_config(home):
    return validate_config(read_json(Path(home) / 'config.json'))


def resolve_api_key(provider):
    if provider.get('api_key_env'):
        variable = provider['api_key_env']
        value = os.environ.get(variable, '').strip()
        if not value:
            raise GatewayError(f'Missing API key environment variable: {variable}')
    else:
        try:
            value = Path(provider['api_key_file']).read_text().strip()
        except (OSError, KeyError):
            raise GatewayError('Cannot read configured API key file') from None
    if not value or any(c.isspace() for c in value):
        raise GatewayError('An API key must be one nonempty token without whitespace')
    return value


def templates_from(path):
    data = read_json(path)
    items = data.get('models') if isinstance(data, dict) else None
    if not isinstance(items, list) or any(not isinstance(m, dict) or not isinstance(m.get('slug'), str) for m in items):
        raise GatewayError('Model catalog must contain a models array with slug fields')
    if len({m['slug'] for m in items}) != len(items):
        raise GatewayError('Model catalog contains duplicate slugs')
    return {'models': items}


def discover_templates(codex_home, catalog_file=None):
    if catalog_file is not None:
        return templates_from(catalog_file)
    cache = codex_home / 'models_cache.json'
    if cache.is_file():
        return templates_from(cache)
    config = codex_home / 'config.toml'
    if config.is_file():
        try:
            source = tomllib.loads(config.read_text()).get('model_catalog_json')
        except (ValueError, OSError):
            source = None
        if source and Path(source).is_file():
            return templates_from(Path(source))
    return {'models': []}


def catalog_for(config, templates):
    lookup = {m['slug']: m for m in templates['models']}
    catalog = []
    for alias, route in config['models'].items():
        template = route['template']
        if template not in lookup:
            raise GatewayError(f'Unknown template {template}; import a Codex model catalog first')
        model = copy.deepcopy(lookup[template])
        model['slug'] = alias
        model['display_name'] = route.get('display_name') or f'{model.get("display_name", template)} · {route["provider"]}'
        model['visibility'] = 'list'
        model['supported_in_api'] = True
        model['upgrade'] = None
        model['availability_nux'] = None
        model['additional_speed_tiers'] = []
        model['service_tiers'] = []
        model['use_responses_lite'] = False
        catalog.append(model)
    return {'models': catalog}


def save_config(home, config):
    home = Path(home)
    validate_config(config)
    catalog = catalog_for(config, templates_from(home / 'templates.json'))
    # Validation completes before either file changes. The live server keeps its
    # existing immutable config until start/run restarts an idle instance.
    write_json(home / 'config.json', config)
    write_json(home / 'models.json', catalog)


def load_catalog(home):
    return templates_from(Path(home) / 'models.json')


def fingerprint(home):
    digest = hashlib.sha256(__version__.encode())
    for name in ('config.json', 'models.json', 'admin-token', 'client-token'):
        try:
            digest.update((Path(home) / name).read_bytes())
        except OSError:
            raise GatewayError(f'Missing gateway file: {name}') from None
    return digest.hexdigest()


def initialize(home, *, codex_home=None, client_auth='codex', port=33989, catalog_file=None, bootstrap_proxy=None):
    home = Path(home).expanduser().resolve()
    if home.exists() and any(home.iterdir()):
        raise GatewayError('Gateway home is not empty; refusing to overwrite existing configuration')
    codex_home = Path(codex_home or os.environ.get('CODEX_HOME', str(Path.home() / '.codex'))).expanduser().resolve()
    config = {'version': 1, 'listen': {'host': '127.0.0.1', 'port': port},
              'codex_home': str(codex_home), 'client_auth': client_auth,
              'default_model': None, 'bootstrap_proxy': bootstrap_proxy,
              'retry_invalid_encrypted_reasoning': False, 'providers': {}, 'models': {}}
    validate_config(config)
    templates = discover_templates(codex_home, catalog_file)
    home.mkdir(mode=0o700, parents=True, exist_ok=True)
    write_json(home / 'templates.json', templates)
    private_write(home / 'admin-token', secrets.token_urlsafe(32) + '\n')
    private_write(home / 'client-token', secrets.token_urlsafe(32) + '\n')
    save_config(home, config)
    return config
