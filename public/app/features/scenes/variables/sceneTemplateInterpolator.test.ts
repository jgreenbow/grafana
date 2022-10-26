import { SceneObjectBase } from '../core/SceneObjectBase';
import { SceneObjectStatePlain } from '../core/types';

import { SceneVariablesManager as SceneVariablesManager, TextBoxSceneVariable } from './SceneVariablesManager';
import { sceneTemplateInterpolator } from './sceneTemplateInterpolator';

interface TestSceneState extends SceneObjectStatePlain {
  nested?: TestScene;
}

class TestScene extends SceneObjectBase<TestSceneState> {}

describe('sceneTemplateInterpolator', () => {
  it('Should be interpolate and use closest variable', () => {
    const scene = new TestScene({
      $variables: new SceneVariablesManager({
        variables: [
          new TextBoxSceneVariable({
            name: 'test',
            value: 'hello',
            text: 'hello',
          }),
          new TextBoxSceneVariable({
            name: 'atRootOnly',
            value: 'RootValue',
            text: 'RootValue',
          }),
        ],
      }),
      nested: new TestScene({
        $variables: new SceneVariablesManager({
          variables: [
            new TextBoxSceneVariable({
              name: 'test',
              value: 'nestedValue',
              text: 'nestedValue',
            }),
          ],
        }),
      }),
    });

    expect(sceneTemplateInterpolator('${test}', scene)).toBe('hello');
    expect(sceneTemplateInterpolator('${test}', scene.state.nested!)).toBe('nestedValue');
    expect(sceneTemplateInterpolator('${atRootOnly}', scene.state.nested!)).toBe('RootValue');
  });
});
