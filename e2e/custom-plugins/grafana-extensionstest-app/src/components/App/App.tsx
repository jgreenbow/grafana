import React from 'react';
import { AppRootProps, PageLayoutType } from '@grafana/data';
import { ActionButton } from 'components/ActionButton';
import { PluginPage, getPluginExtensions } from '@grafana/runtime';
import { Stack } from '@grafana/ui';

type AppExtensionContext = {};

export class App extends React.PureComponent<AppRootProps> {
  render() {
    const extensionPointId = 'plugins/grafana-extensionstest-app/actions';
    const context: AppExtensionContext = {};

    const { extensions } = getPluginExtensions({
      extensionPointId,
      context,
    });

    return (
      <PluginPage layout={PageLayoutType.Standard}>
        <h1>UI extensions</h1>
        <Stack direction={'column'} gap={4}>
          <section>
            <h2>Legacy APIs</h2>
            <p>Showcase that legacy APIs such as configureExtensionLink and getPluginExtensions.</p>
            <ActionButton extensions={extensions} />
          </section>
          <section>
            <h2>New API</h2>
            <p>Showcase that legacy APIs such as configureExtensionLink and getPluginExtensions.</p>
            <ActionButton extensions={extensions} />
          </section>
        </Stack>
      </PluginPage>
    );
  }
}
