import { useState } from 'react';
import { useAsync } from 'react-use';
import SwaggerUI from 'swagger-ui-react';

import { createTheme, SelectableValue } from '@grafana/data';
import { Stack, Select } from '@grafana/ui';
import { ThemeProvider } from 'app/core/utils/ConfigProvider';

export const Page = () => {
  const theme = createTheme({ colors: { mode: 'light' } });
  const [url, setURL] = useState<SelectableValue<string>>();
  const urls = useAsync(async () => {
    const v2 = { label: 'Grafana API (OpenAPI v2)', value: 'public/api-merged.json' };
    const v3 = { label: 'Grafana API (OpenAPI v3)', value: 'public/openapi3.json' };
    const urls: Array<SelectableValue<string>> = [v2, v3];

    const rsp = await fetch('openapi/v3');
    const apis = await rsp.json();
    for (const [key, val] of Object.entries<any>(apis.paths)) {
      const parts = key.split('/');
      if (parts.length === 3) {
        urls.push({
          label: `${parts[1]}/${parts[2]}`,
          value: val.serverRelativeURL.substring(1), // remove initial slash
        });
      }
    }

    let idx = 0;
    const urlParams = new URLSearchParams(window.location.search);
    const api = urlParams.get('api');
    if (api) {
      urls.forEach((url, i) => {
        if (url.label === api) {
          idx = i;
        }
      });
    }
    setURL(urls[idx]);
    return urls;
  });

  return (
    <div>
      <ThemeProvider value={theme}>
        <div className="topbar" style={{ backgroundColor: '#000', padding: '10px' }}>
          <Stack justifyContent={'space-between'}>
            <img height="40" src="public/img/grafana_icon.svg" alt="Grafana" />
            <Select
              options={urls.value}
              onChange={(v) => {
                const url = new URL(window.location.href);
                url.searchParams.set('api', v.label ?? '');
                url.hash = '';
                history.pushState(null, '', url);
                setURL(v);
              }}
              value={url}
              isLoading={urls.loading}
            />
          </Stack>
        </div>

        {url?.value && <SwaggerUI url={url.value} deepLinking={true} tryItOutEnabled={true} />}
      </ThemeProvider>
    </div>
  );
};
