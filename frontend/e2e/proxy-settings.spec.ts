import { expect, test, type Page, type Response } from '@playwright/test';

type CallRequest = {
  method: string;
  engine: string;
  params: Record<string, unknown>;
};

const settingsWithoutEmptyProxyFields = {
  version: 1,
  dataRoot: 'C:\\ProgramData\\UnifiedContainerDesktop',
  windowsEndpoint: '',
  helperURL: '',
  caCert: '',
  clientCert: '',
  clientKey: '',
  wsl: {
    distro: 'UCD-Linux',
    serviceAccount: 'UcdService',
    cpus: 4,
    memoryMB: 4096,
    diskGB: 128,
    profileDirectory: 'C:\\ProgramData\\profile',
    installDirectory: 'C:\\ProgramData\\wsl',
  },
  projects: [],
  dockerProxy: {},
  containersProxy: {},
  network: {
    defaultNetworkingMode: 'ipv4only',
    dnsInhibition: 'auto',
    defaultPortBinding: 'local',
    hostNetworkingEnabled: false,
  },
  resourceSaver: { enabled: false, idleSeconds: 300 },
  registryMirrors: [],
};

async function useWebTransport(page: Page) {
  const calls: CallRequest[] = [];
  let bearerAuthorizationSeen = false;

  await page.addInitScript(() => {
    sessionStorage.setItem('ucd-web-token', 'T'.repeat(43));
  });

  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.pathname !== '/' || request.method() !== 'GET') {
      await route.fallback();
      return;
    }

    const response = await route.fetch();
    const html = (await response.text()).replace(
      '</head>',
      '<meta name="ucd-transport" content="web"></head>',
    );
    const headers = response.headers();
    delete headers['content-length'];
    delete headers['content-encoding'];
    await route.fulfill({ status: response.status(), headers, body: html });
  });

  await page.route('**/v1/call', async (route) => {
    const request = route.request();
    bearerAuthorizationSeen ||=
      request.headers().authorization === `Bearer ${'T'.repeat(43)}`;
    const call = request.postDataJSON() as CallRequest;
    calls.push(call);

    let result: unknown;
    switch (call.method) {
      case 'system.settings':
        result = {
          settings: settingsWithoutEmptyProxyFields,
          principal: {
            sid: 'S-1',
            name: 'Browser test admin',
            role: 'administrator',
          },
        };
        break;
      case 'system.events':
        result = { epoch: 'proxy-settings-test', events: [] };
        break;
      case 'engine.list':
        result = [];
        break;
      case 'container.list':
        result = { items: [], errors: [] };
        break;
      case 'engine.status':
        result = {
          id: 'linux',
          os: 'linux',
          online: true,
          containers: 0,
          running: 0,
          stopped: 0,
          cpus: 4,
          memoryBytes: 4096 * 1024 * 1024,
          resourceSaver: false,
          activeOperations: 0,
        };
        break;
      case 'wsl.status':
        result = {
          distro: 'UCD-Linux',
          running: true,
          cpus: 4,
          memoryMB: 4096,
          diskGB: 128,
          memoryUsedMB: 0,
          diskUsedBytes: 0,
          restartRequired: false,
        };
        break;
      case 'operator.list':
      case 'share.list':
        result = [];
        break;
      case 'system.configure':
        result = { restartRequired: true };
        break;
      default:
        await route.fulfill({
          status: 500,
          contentType: 'application/json',
          body: JSON.stringify({
            error: {
              code: 'unexpected_test_call',
              message: `Unexpected API call: ${call.method}`,
            },
          }),
        });
        return;
    }

    await route.fulfill({
      status: 200,
      contentType: 'application/json',
      body: JSON.stringify({ result }),
    });
  });

  return { calls, bearerAuthorizationSeen: () => bearerAuthorizationSeen };
}

function isConfigureResponse(response: Response) {
  if (!response.url().endsWith('/v1/call')) return false;
  try {
    return response.request().postDataJSON()?.method === 'system.configure';
  } catch {
    return false;
  }
}

async function waitForConfigureResponse(
  page: Page,
  form: ReturnType<Page['locator']>,
) {
  try {
    return await page.waitForResponse(isConfigureResponse, { timeout: 5000 });
  } catch {
    const feedback = await form
      .getByRole('alert')
      .innerText()
      .catch(() => '(no form feedback)');
    throw new Error(
      `No system.configure response was sent; settings form feedback: ${feedback}`,
    );
  }
}

async function openSettings(page: Page) {
  await page.goto('/');
  await expect(page.getByText('Controller connected')).toBeVisible();
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.getByRole('button', { name: 'Proxies', exact: true }).click();
  await expect(
    page.getByRole('heading', {
      name: 'Proxies and registry mirrors',
      exact: true,
    }),
  ).toBeVisible();
}

test('saves the shared HTTP proxy defaults when the controller omits empty proxy fields', async ({
  page,
}) => {
  const transport = await useWebTransport(page);
  await openSettings(page);

  const dockerProxy = {
    http: 'http://127.0.0.1:43127',
    https: 'http://127.0.0.1:43127',
    socks5: '',
    noProxy: 'ucdproxy.test',
  };
  const containersProxy = {
    http: 'http://ucdproxy:5000',
    https: 'http://ucdproxy:5000',
    socks5: '',
    noProxy: 'ucddirect',
  };
  const mirrors = ['http://127.0.0.1:43127'];
  const fields = [
    ['Docker proxy HTTP URL', dockerProxy.http],
    ['Docker proxy HTTPS URL', dockerProxy.https],
    ['Docker proxy SOCKS5 URL', dockerProxy.socks5],
    ['Docker proxy Bypass list', dockerProxy.noProxy],
    ['Container proxy HTTP URL', containersProxy.http],
    ['Container proxy HTTPS URL', containersProxy.https],
    ['Container proxy SOCKS5 URL', containersProxy.socks5],
    ['Container proxy Bypass list', containersProxy.noProxy],
  ] as const;
  for (const [label, value] of fields) {
    await page.getByLabel(label, { exact: true }).fill(value);
  }
  await page.getByLabel(/Registry mirrors/).fill(mirrors.join('\n'));

  const form = page
    .locator('form')
    .filter({ has: page.getByRole('button', { name: 'Save proxy settings' }) });
  const responsePromise = waitForConfigureResponse(page, form);
  await page
    .getByRole('button', { name: 'Save proxy settings', exact: true })
    .click();
  const response = await responsePromise;
  expect(response.ok()).toBe(true);
  expect(response.request().postDataJSON()).toMatchObject({
    method: 'system.configure',
    engine: '',
    params: {
      dockerProxy,
      containersProxy,
      registryMirrors: mirrors,
    },
  });
  expect(
    transport.calls.some((call) => call.method === 'system.configure'),
  ).toBe(true);
  expect(transport.bearerAuthorizationSeen()).toBe(true);
  await expect(
    form.getByRole('status').filter({ hasText: /^Saved\./ }),
  ).toContainText('A Docker engine restart is required');
});

test('saves network defaults when the controller omits its empty bridge subnet', async ({
  page,
}) => {
  const transport = await useWebTransport(page);
  await openSettings(page);

  await page.getByRole('button', { name: 'Network', exact: true }).click();
  await page.getByLabel('Default networking mode').selectOption('dual-stack');
  const form = page.locator('form').filter({
    has: page.getByRole('button', { name: 'Save network defaults' }),
  });
  const responsePromise = waitForConfigureResponse(page, form);
  await page
    .getByRole('button', { name: 'Save network defaults', exact: true })
    .click();
  const response = await responsePromise;
  expect(response.ok()).toBe(true);
  expect(response.request().postDataJSON()).toMatchObject({
    method: 'system.configure',
    engine: '',
    params: {
      network: {
        dockerSubnet: '',
        defaultNetworkingMode: 'dual-stack',
        dnsInhibition: 'auto',
        defaultPortBinding: 'local',
        hostNetworkingEnabled: false,
      },
    },
  });
  expect(
    transport.calls.some((call) => call.method === 'system.configure'),
  ).toBe(true);
  expect(transport.bearerAuthorizationSeen()).toBe(true);
  await expect(
    form.getByRole('status').filter({ hasText: /^Saved\./ }),
  ).toContainText('needs a restart to apply bridge and DNS defaults');
});
