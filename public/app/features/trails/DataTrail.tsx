import { css } from '@emotion/css';
import { useEffect } from 'react';

import { AdHocVariableFilter, GrafanaTheme2, PageLayoutType, RawTimeRange, VariableHide, urlUtil } from '@grafana/data';
import { locationService, useChromeHeaderHeight } from '@grafana/runtime';
import {
  AdHocFiltersVariable,
  DataSourceVariable,
  getUrlSyncManager,
  SceneComponentProps,
  SceneControlsSpacer,
  sceneGraph,
  SceneObject,
  SceneObjectBase,
  SceneObjectState,
  SceneObjectUrlSyncConfig,
  SceneObjectUrlValues,
  SceneRefreshPicker,
  SceneTimePicker,
  SceneTimeRange,
  sceneUtils,
  SceneVariable,
  SceneVariableSet,
  SceneVariableState,
  VariableDependencyConfig,
  VariableValueSelectors,
} from '@grafana/scenes';
import { useStyles2 } from '@grafana/ui';
import { Page } from 'app/core/components/Page/Page';

import { DataTrailSettings } from './DataTrailSettings';
import { DataTrailHistory } from './DataTrailsHistory';
import { MetricScene } from './MetricScene';
import { MetricSelectScene } from './MetricSelect/MetricSelectScene';
import { MetricsHeader } from './MetricsHeader';
import { getTrailStore } from './TrailStore/TrailStore';
import { MetricDatasourceHelper } from './helpers/MetricDatasourceHelper';
import { reportChangeInLabelFilters } from './interactions';
import { getOtelResources, isOtelStandardization } from './otel/api';
import { OtelTargetType } from './otel/types';
import {
  MetricSelectedEvent,
  trailDS,
  VAR_DATASOURCE,
  VAR_DATASOURCE_EXPR,
  VAR_FILTERS,
  VAR_OTEL_RESOURCES,
} from './shared';
import { getMetricName, getTrailFor } from './utils';

export interface DataTrailState extends SceneObjectState {
  topScene?: SceneObject;
  embedded?: boolean;
  controls: SceneObject[];
  history: DataTrailHistory;
  settings: DataTrailSettings;
  createdAt: number;

  // just for for the starting data source
  initialDS?: string;
  initialFilters?: AdHocVariableFilter[];

  // this is for otel, if the data source has it, it will be updated here
  otelTargets?: OtelTargetType[];
  otelResources?: string[];
  // Synced with url
  metric?: string;
  metricSearch?: string;
}

export class DataTrail extends SceneObjectBase<DataTrailState> {
  protected _urlSync = new SceneObjectUrlSyncConfig(this, { keys: ['metric', 'metricSearch'] });

  public constructor(state: Partial<DataTrailState>) {
    super({
      $timeRange: state.$timeRange ?? new SceneTimeRange({}),
      $variables: state.$variables ?? getVariableSet(state.initialDS, state.metric, state.initialFilters),
      controls: state.controls ?? [
        new VariableValueSelectors({ layout: 'vertical' }),
        new SceneControlsSpacer(),
        new SceneTimePicker({}),
        new SceneRefreshPicker({}),
      ],
      history: state.history ?? new DataTrailHistory({}),
      settings: state.settings ?? new DataTrailSettings({}),
      createdAt: state.createdAt ?? new Date().getTime(),
      ...state,
    });

    this.addActivationHandler(this._onActivate.bind(this));
  }

  public _onActivate() {
    if (!this.state.topScene) {
      this.setState({ topScene: getTopSceneFor(this.state.metric) });
    }

    // Some scene elements publish this
    this.subscribeToEvent(MetricSelectedEvent, this._handleMetricSelectedEvent.bind(this));

    const filtersVariable = sceneGraph.lookupVariable(VAR_FILTERS, this);
    if (filtersVariable instanceof AdHocFiltersVariable) {
      this._subs.add(
        filtersVariable?.subscribeToState((newState, prevState) => {
          if (!this._addingFilterWithoutReportingInteraction) {
            reportChangeInLabelFilters(newState.filters, prevState.filters);
          }
        })
      );
    }

    // Save the current trail as a recent if the browser closes or reloads
    const saveRecentTrail = () => getTrailStore().setRecentTrail(this);
    window.addEventListener('unload', saveRecentTrail);

    return () => {
      if (!this.state.embedded) {
        saveRecentTrail();
      }
      window.removeEventListener('unload', saveRecentTrail);
    };
  }

  protected _variableDependency = new VariableDependencyConfig(this, {
    variableNames: [VAR_DATASOURCE, VAR_OTEL_RESOURCES],
    onReferencedVariableValueChanged: async (variable: SceneVariable) => {
      const { name } = variable.state;

      if (name === VAR_DATASOURCE) {
        this.datasourceHelper.reset();
        this.checkOtelStandardization();
      }
      // if the variable is the otel deployment_environment
      // filter with no other filters
      // then we load all the other resources
      const value = variable.getValue()?.toLocaleString();
      // additional filters will be a comma separated list
      const isDeploymentEnvironment = value?.includes('deployment_environment');
      if (name === VAR_OTEL_RESOURCES && isDeploymentEnvironment) {
        this.loadOtelResources();
      }
    },
  });

  /**
   * Assuming that the change in filter was already reported with a cause other than `'adhoc_filter'`,
   * this will modify the adhoc filter variable and prevent the automatic reporting which would
   * normally occur through the call to `reportChangeInLabelFilters`.
   */
  public addFilterWithoutReportingInteraction(filter: AdHocVariableFilter) {
    const variable = sceneGraph.lookupVariable('filters', this);
    if (!(variable instanceof AdHocFiltersVariable)) {
      return;
    }

    this._addingFilterWithoutReportingInteraction = true;

    variable.setState({ filters: [...variable.state.filters, filter] });

    this._addingFilterWithoutReportingInteraction = false;
  }

  private _addingFilterWithoutReportingInteraction = false;
  private datasourceHelper = new MetricDatasourceHelper(this);

  public getMetricMetadata(metric?: string) {
    return this.datasourceHelper.getMetricMetadata(metric);
  }

  public getCurrentMetricMetadata() {
    return this.getMetricMetadata(this.state.metric);
  }

  public restoreFromHistoryStep(state: DataTrailState) {
    if (!state.topScene && !state.metric) {
      // If the top scene for an  is missing, correct it.
      state.topScene = new MetricSelectScene({});
    }

    this.setState(
      sceneUtils.cloneSceneObjectState(state, {
        history: this.state.history,
        metric: !state.metric ? undefined : state.metric,
        metricSearch: !state.metricSearch ? undefined : state.metricSearch,
      })
    );

    const urlState = getUrlSyncManager().getUrlState(this);
    const fullUrl = urlUtil.renderUrl(locationService.getLocation().pathname, urlState);
    locationService.replace(fullUrl);
  }

  private _handleMetricSelectedEvent(evt: MetricSelectedEvent) {
    this.setState(this.getSceneUpdatesForNewMetricValue(evt.payload));

    // Add metric to adhoc filters baseFilter
    const filterVar = sceneGraph.lookupVariable(VAR_FILTERS, this);
    if (filterVar instanceof AdHocFiltersVariable) {
      filterVar.setState({
        baseFilters: getBaseFiltersForMetric(evt.payload),
      });
    }
  }

  private getSceneUpdatesForNewMetricValue(metric: string | undefined) {
    const stateUpdate: Partial<DataTrailState> = {};
    stateUpdate.metric = metric;
    stateUpdate.topScene = getTopSceneFor(metric);
    return stateUpdate;
  }

  getUrlState() {
    const { metric, metricSearch } = this.state;
    return { metric, metricSearch };
  }

  updateFromUrl(values: SceneObjectUrlValues) {
    const stateUpdate: Partial<DataTrailState> = {};

    if (typeof values.metric === 'string') {
      if (this.state.metric !== values.metric) {
        Object.assign(stateUpdate, this.getSceneUpdatesForNewMetricValue(values.metric));
      }
    } else if (values.metric == null) {
      stateUpdate.metric = undefined;
      stateUpdate.topScene = new MetricSelectScene({});
    }

    if (typeof values.metricSearch === 'string') {
      stateUpdate.metricSearch = values.metricSearch;
    } else if (values.metric == null) {
      stateUpdate.metricSearch = undefined;
    }

    this.setState(stateUpdate);
  }

  public async checkOtelStandardization() {
    // call up in to the parent trail
    const trail = getTrailFor(this);

    const otelResourcesVariable = sceneGraph.lookupVariable(VAR_OTEL_RESOURCES, this);

    // get the time range
    const timeRange: RawTimeRange | undefined = trail.state.$timeRange?.state;

    if (timeRange) {
      const datasourceUid = sceneGraph.interpolate(trail, VAR_DATASOURCE_EXPR);
      const isStandard = await isOtelStandardization(datasourceUid, timeRange);

      if (isStandard) {
        // @ts-ignore this is to update defaultKeys which exists but ts says it doesn't
        if (otelResourcesVariable?.state.filters.length === 0) {
          // load the first var, deployment_environment
          otelResourcesVariable?.setState({
            // @ts-ignore this is to update defaultKeys which exists but ts says it doesn't
            defaultKeys: [{ text: 'deployment_environment' }],
            hide: VariableHide.hideLabel,
          });
        } else {
          // there are already otel filters so we load the regular filters
          const excludedFilters = getExcludedOtelFilters(otelResourcesVariable);
          const resources = await getOtelResources(datasourceUid, timeRange, excludedFilters);
          if (resources.length === 0) {
            return;
          }

          const otelLabels = resources.map((resource) => {
            return { text: resource };
          });

          this.setState({ otelResources: resources });

          otelResourcesVariable?.setState({
            // @ts-ignore this is to update defaultKeys which exists but ts says it doesn't
            defaultKeys: otelLabels,
            hide: VariableHide.hideLabel,
          });
        }
      } else {
        // @ts-ignore this is to update defaultKeys which exists but ts says it doesn't
        otelResourcesVariable?.setState({ defaultKeys: [], hide: VariableHide.hideVariable });
      }
    }
  }

  public async loadOtelResources() {
    // call up in to the parent trail
    const trail = getTrailFor(this);
    // get the otel resources variable
    const otelResourcesVariable = sceneGraph.lookupVariable(VAR_OTEL_RESOURCES, this);
    // get the time range
    const timeRange: RawTimeRange | undefined = trail.state.$timeRange?.state;

    if (timeRange) {
      // get the data source UID for making calls to the DS
      const datasourceUid = sceneGraph.interpolate(trail, VAR_DATASOURCE_EXPR);
      const excludedFilters = getExcludedOtelFilters(otelResourcesVariable);
      // get a list of labels for target info
      const resources = await getOtelResources(datasourceUid, timeRange, excludedFilters);

      if (resources.length === 0) {
        return;
      }

      const otelLabels = resources.map((resource) => {
        return { text: resource };
      });

      this.setState({ otelResources: resources });

      // @ts-ignore this is to update defaultKeys which exists but ts says it doesn't
      otelResourcesVariable?.setState({ defaultKeys: otelLabels, hide: VariableHide.hideLabel });
    }
  }

  static Component = ({ model }: SceneComponentProps<DataTrail>) => {
    useEffect(() => {
      model.checkOtelStandardization();
    }, [model]);

    const { controls, topScene, history, settings, metric } = model.useState();
    const chromeHeaderHeight = useChromeHeaderHeight();
    const styles = useStyles2(getStyles, chromeHeaderHeight ?? 0);
    const showHeaderForFirstTimeUsers = getTrailStore().recent.length < 2;

    return (
      <Page navId="explore/metrics" pageNav={{ text: getMetricName(metric) }} layout={PageLayoutType.Custom}>
        <div className={styles.container}>
          {showHeaderForFirstTimeUsers && <MetricsHeader />}
          <history.Component model={history} />
          {controls && (
            <div className={styles.controls}>
              {controls.map((control) => (
                <control.Component key={control.state.key} model={control} />
              ))}
              <settings.Component model={settings} />
            </div>
          )}
          <div className={styles.body}>{topScene && <topScene.Component model={topScene} />}</div>
        </div>
      </Page>
    );
  };
}

export function getTopSceneFor(metric?: string) {
  if (metric) {
    return new MetricScene({ metric: metric });
  } else {
    return new MetricSelectScene({});
  }
}

function getVariableSet(initialDS?: string, metric?: string, initialFilters?: AdHocVariableFilter[]) {
  return new SceneVariableSet({
    variables: [
      new DataSourceVariable({
        name: VAR_DATASOURCE,
        label: 'Data source',
        description: 'Only prometheus data sources are supported',
        value: initialDS,
        pluginId: 'prometheus',
      }),
      new AdHocFiltersVariable({
        name: VAR_FILTERS,
        addFilterButtonText: 'Add label',
        datasource: trailDS,
        hide: VariableHide.hideLabel,
        layout: 'vertical',
        filters: initialFilters ?? [],
        baseFilters: getBaseFiltersForMetric(metric),
      }),
      new AdHocFiltersVariable({
        name: VAR_OTEL_RESOURCES,
        addFilterButtonText: 'Otel resources',
        datasource: trailDS,
        hide: VariableHide.hideVariable,
        layout: 'vertical',
        filters: initialFilters ?? [],
        defaultKeys: [],
      }),
    ],
  });
}

function getStyles(theme: GrafanaTheme2, chromeHeaderHeight: number) {
  return {
    container: css({
      flexGrow: 1,
      display: 'flex',
      gap: theme.spacing(1),
      flexDirection: 'column',
      background: theme.isLight ? theme.colors.background.primary : theme.colors.background.canvas,
      padding: theme.spacing(2, 3, 2, 3),
    }),
    body: css({
      flexGrow: 1,
      display: 'flex',
      flexDirection: 'column',
    }),
    controls: css({
      display: 'flex',
      gap: theme.spacing(1),
      padding: theme.spacing(1, 0),
      alignItems: 'flex-end',
      flexWrap: 'wrap',
      position: 'sticky',
      background: theme.isDark ? theme.colors.background.canvas : theme.colors.background.primary,
      zIndex: theme.zIndex.navbarFixed,
      top: chromeHeaderHeight,
    }),
  };
}

function getBaseFiltersForMetric(metric?: string): AdHocVariableFilter[] {
  if (metric) {
    return [{ key: '__name__', operator: '=', value: metric }];
  }
  return [];
}

function getExcludedOtelFilters(variable: SceneVariable<SceneVariableState> | null) {
  if (!variable) {
    return [];
  }
  // @ts-ignore
  return variable?.state.filters.map((f) => f.key);
}
