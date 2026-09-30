"""Command-line configuration and process management."""

import argparse
import json
from pathlib import Path
import sys

from . import __version__
from .config import (GatewayError, catalog_for, default_home, initialize, load_config,
                     resolve_api_key, save_config, templates_from, write_json)


def parser():
    root = argparse.ArgumentParser(description='Route Codex to multiple Responses-compatible providers.')
    root.add_argument('--version', action='version', version=__version__)
    root.add_argument('--home', type=Path, default=None, help='Gateway config directory (or CODEX_GATEWAY_HOME)')
    commands = root.add_subparsers(dest='command', required=True)
    init = commands.add_parser('init', help='Create private configuration without changing Codex settings')
    init.add_argument('--codex-home', type=Path)
    init.add_argument('--auth-mode', choices=['codex', 'token'], default='codex',
                      help='codex: existing Codex login; token: API providers without ChatGPT login')
    init.add_argument('--port', type=int, default=33989)
    init.add_argument('--catalog', type=Path, help='Codex models_cache.json or a custom model catalog')
    init.add_argument('--bootstrap-proxy', help='Optional HTTP proxy for Codex account bootstrap')

    provider = commands.add_parser('provider', help='Manage providers').add_subparsers(dest='action', required=True)
    add = provider.add_parser('add')
    add.add_argument('name')
    add.add_argument('--base-url', required=True, help='Base URL whose /responses endpoint accepts Codex requests')
    add.add_argument('--auth', choices=['api_key', 'codex'], default='api_key')
    keys = add.add_mutually_exclusive_group()
    keys.add_argument('--api-key-env', help='Environment variable name; never the key value')
    keys.add_argument('--api-key-file', type=Path, help='Private file containing only the API key')
    add.add_argument('--proxy', help='Explicit HTTP proxy; omitted means direct connection')
    add.add_argument('--allow-insecure-http', action='store_true', help='Allow HTTP on a trusted remote network')
    add.add_argument('--replace', action='store_true')
    provider.add_parser('list')
    provider.add_parser('remove').add_argument('name')

    model = commands.add_parser('model', help='Manage model aliases and upstream mappings').add_subparsers(dest='action', required=True)
    add = model.add_parser('add')
    add.add_argument('alias')
    add.add_argument('--provider', required=True)
    add.add_argument('--upstream-model', required=True)
    add.add_argument('--template', required=True, help='Existing Codex model slug whose capabilities match this upstream')
    add.add_argument('--display-name')
    add.add_argument('--default', action='store_true')
    add.add_argument('--replace', action='store_true')
    model.add_parser('list')
    model.add_parser('remove').add_argument('alias')

    catalog = commands.add_parser('catalog', help='Import Codex model metadata').add_subparsers(dest='action', required=True)
    catalog.add_parser('import').add_argument('path', type=Path)
    catalog.add_parser('list')
    catalog.add_parser('build')

    config = commands.add_parser('config', help='Inspect or update gateway settings').add_subparsers(dest='action', required=True)
    config.add_parser('show')
    update = config.add_parser('set')
    update.add_argument('key', choices=['listen.port', 'codex_home', 'client_auth', 'default_model',
                                      'bootstrap_proxy', 'retry_invalid_encrypted_reasoning'])
    update.add_argument('value', help='String, integer, true, false, or null')
    validate = commands.add_parser('validate', help='Validate config, templates and optional credential availability')
    validate.add_argument('--credentials', action='store_true')

    for name, help_text in [('start', 'Start or reuse an idle-compatible background gateway'),
                            ('serve', 'Run the gateway in the foreground'),
                            ('status', 'Inspect this gateway'), ('stop', 'Stop an idle gateway')]:
        commands.add_parser(name, help=help_text)
    run = commands.add_parser('run', help='Start the gateway and launch Codex with isolated command-line settings')
    run.add_argument('--codex-bin', help='Codex executable or existing launcher (defaults to PATH)')
    run.add_argument('codex_args', nargs=argparse.REMAINDER, help='Codex arguments after --, e.g. -- resume')
    return root


def output(value):
    print(json.dumps(value, ensure_ascii=False, indent=2))


def dispatch(args):
    home = args.home.expanduser().resolve() if args.home is not None else default_home()
    if args.command == 'init':
        initialize(home, codex_home=args.codex_home, client_auth=args.auth_mode,
                   port=args.port, catalog_file=args.catalog, bootstrap_proxy=args.bootstrap_proxy)
        output({'home': str(home), 'initialized': True,
                'templates': len(templates_from(home / 'templates.json')['models'])})
        return 0
    config = load_config(home)
    if args.command == 'provider':
        if args.action == 'list':
            output(config['providers'])
            return 0
        if args.action == 'add':
            if args.name in config['providers'] and not args.replace:
                raise GatewayError('Provider already exists; use --replace to update it')
            item = {'base_url': args.base_url.rstrip('/'), 'auth': args.auth}
            if args.api_key_env:
                item['api_key_env'] = args.api_key_env
            if args.api_key_file:
                item['api_key_file'] = str(args.api_key_file.expanduser().resolve())
            if args.proxy:
                item['proxy'] = args.proxy
            if args.allow_insecure_http:
                item['allow_insecure_http'] = True
            config['providers'][args.name] = item
        else:
            if args.name not in config['providers']:
                raise GatewayError('Unknown provider')
            if any(m['provider'] == args.name for m in config['models'].values()):
                raise GatewayError('Provider still has model aliases; remove those models first')
            del config['providers'][args.name]
        save_config(home, config)
        output({'provider': args.name, 'action': args.action})
    elif args.command == 'model':
        if args.action == 'list':
            output({'default_model': config.get('default_model'), 'models': config['models']})
            return 0
        if args.action == 'add':
            if args.alias in config['models'] and not args.replace:
                raise GatewayError('Model alias already exists; use --replace to update it')
            item = {'provider': args.provider, 'model': args.upstream_model, 'template': args.template}
            if args.display_name:
                item['display_name'] = args.display_name
            config['models'][args.alias] = item
            if args.default or config.get('default_model') is None:
                config['default_model'] = args.alias
        else:
            if args.alias not in config['models']:
                raise GatewayError('Unknown model alias')
            del config['models'][args.alias]
            if config.get('default_model') == args.alias:
                config['default_model'] = next(iter(config['models']), None)
        save_config(home, config)
        output({'model': args.alias, 'action': args.action})
    elif args.command == 'catalog':
        old = templates_from(home / 'templates.json')
        if args.action == 'list':
            output([{'slug': m['slug'], 'display_name': m.get('display_name', m['slug'])} for m in old['models']])
            return 0
        if args.action == 'import':
            imported = templates_from(args.path.expanduser().resolve())
            merged = {m['slug']: m for m in old['models']}
            merged.update({m['slug']: m for m in imported['models']})
            new = {'models': list(merged.values())}
            catalog_for(config, new)
            write_json(home / 'templates.json', new)
        save_config(home, config)
        output({'models': len(config['models']), 'catalog': str(home / 'models.json')})
    elif args.command == 'config':
        if args.action == 'show':
            output(config)
            return 0
        try:
            value = json.loads(args.value)
        except ValueError:
            value = args.value
        if args.key == 'codex_home':
            if not isinstance(value, str) or not value:
                raise GatewayError('codex_home must be a nonempty path string')
            value = str(Path(value).expanduser().resolve())
        if args.key == 'listen.port':
            config['listen']['port'] = value
        else:
            config[args.key] = value
        save_config(home, config)
        output({'updated': args.key})
    elif args.command == 'validate':
        catalog_for(config, templates_from(home / 'templates.json'))
        if args.credentials:
            for provider in config['providers'].values():
                if provider['auth'] == 'api_key':
                    resolve_api_key(provider)
        output({'valid': True, 'providers': len(config['providers']), 'models': len(config['models'])})
    elif args.command == 'serve':
        from .server import serve
        serve(home)
    else:
        from . import lifecycle
        if args.command in ('start', 'run'):
            save_config(home, config)
        if args.command == 'run':
            if not config['models']:
                raise GatewayError('No models configured; add a provider and model alias first')
            forwarded = args.codex_args
            if forwarded and forwarded[0] == '--':
                forwarded = forwarded[1:]
            return lifecycle.run_codex(home, forwarded, codex_executable=args.codex_bin)
        output(getattr(lifecycle, args.command)(home))
    return 0


def main(argv=None):
    args = parser().parse_args(argv)
    try:
        return dispatch(args)
    except (GatewayError, OSError) as error:
        print(f'codex-gateway: {error}', file=sys.stderr)
        return 1
    except KeyboardInterrupt:
        return 130
