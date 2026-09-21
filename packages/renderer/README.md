# Web AGenUI Renderer

`agenui-studio-renderer` is a lightweight, dependency-free web renderer for
the complete AGenUI v0.9 basic catalog. It powers the live preview in the
`web/` admin console and can be used standalone to render AGenUI protocol in a
browser.

## Scope

- Protocol application: `createSurface`, `updateComponents` (upsert by id),
  `updateDataModel`, `deleteComponents`
- Components: `AudioPlayer`, `Button`, `Card`, `CheckBox`, `ChoicePicker`,
  `Column`, `DateTimeInput`, `Divider`, `Icon`, `Image`, `List`, `Modal`,
  `Row`, `Slider`, `Tabs`, `Text`, `TextField`, `Video`. Unknown component
  types still render as labeled boxes for forward-compatible inspection.
- Data binding: `{"path": "..."}` property bindings with list-item scopes
  (`List` template children + `path`)
- Catalog style extension: box model, flex, typography, radius, colors and opacity
- Actions: `onAction(event, componentId)` callback

## Usage

```ts
import { Surface, renderSurface } from "agenui-studio-renderer";
import "agenui-studio-renderer/agenui.css";

const surface = new Surface();
surface.reset(pkg.protocol); // protocol from a Card Execution Package
renderSurface(document.getElementById("preview")!, surface, {
  onAction: (event, componentId) => console.log(event, componentId),
});
```

## Build

```bash
npm install
npm run build   # emits dist/ (ESM + type declarations)
```

## Catalog compatibility

The default `renderer-catalogs/1.1.0` release uses the complete v0.9 basic
catalog. Applications may publish a smaller catalog when their own renderer
does not implement every component.
