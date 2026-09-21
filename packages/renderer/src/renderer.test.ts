import { describe, expect, it } from "vitest";
import { JSDOM } from "jsdom";

import { Surface } from "./surface";
import { renderSurface } from "./render";
import type { AGenUIMessage } from "./protocol";

describe("AGenUI v0.9 renderer", () => {
  it("applies updateDataModel value at JSON Pointer path", () => {
    const surface = new Surface();
    surface.apply([
      {
        version: "v0.9",
        updateDataModel: {
          surfaceId: "default",
          path: "/product",
          value: { title: "城市博物馆通票", price: "¥688起" },
        },
      },
    ] as AGenUIMessage[]);

    expect(surface.data).toEqual({
      product: { title: "城市博物馆通票", price: "¥688起" },
    });
  });

  it("renders source AGenUI components, bindings, styles and actions", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, {
      document: dom.window.document,
      HTMLElement: dom.window.HTMLElement,
    });

    const surface = new Surface();
    surface.reset([
      {
        version: "v0.9",
        createSurface: {
          surfaceId: "default",
          catalogId: "https://agenui.org/specification/v0_9/catalog.json",
        },
      },
      {
        version: "v0.9",
        updateComponents: {
          surfaceId: "default",
          components: [
            {
              id: "root",
              component: "Column",
              children: ["title", "action"],
              styles: {
                backgroundColor: "#ffffff",
                padding: "16px",
              },
            },
            {
              id: "title",
              component: "Text",
              text: { path: "/product/title" },
              styles: {
                color: "#111827",
                fontSize: "18px",
              },
            },
            {
              id: "action_label",
              component: "Text",
              text: "查看详情",
            },
            {
              id: "action",
              component: "Button",
              child: "action_label",
              action: { event: { name: "product.view" } },
            },
          ],
        },
      },
      {
        version: "v0.9",
        updateDataModel: {
          surfaceId: "default",
          path: "/product",
          value: { title: "城市博物馆通票" },
        },
      },
    ] as AGenUIMessage[]);

    const actions: string[] = [];
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface, {
      onAction: (event) => actions.push(event),
    });

    expect(container.textContent).toContain("城市博物馆通票");
    expect(container.textContent).toContain("查看详情");
    expect((container.firstElementChild as HTMLElement).style.backgroundColor).not.toBe("");
    expect((container.querySelector('[data-component-id="title"]') as HTMLElement).style.color).not.toBe("");
    (container.querySelector("button") as HTMLButtonElement).click();
    expect(actions).toEqual(["product.view"]);
  });

  it("applies the catalog-declared kebab-case style keys", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, { document: dom.window.document, HTMLElement: dom.window.HTMLElement });
    const surface = new Surface();
    surface.reset([{ version: "v0.9", updateComponents: { surfaceId: "default", components: [
      { id: "root", component: "Text", text: "样式", styles: {
        "font-size": "18px", "font-weight": 600, "line-height": 1.4,
        "background-color": "#ffffff", "border-radius": "8px", opacity: 0.8,
      } },
    ] } }] as AGenUIMessage[]);
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    const text = container.firstElementChild as HTMLElement;
    expect(text.style.fontSize).toBe("18px");
    expect(text.style.fontWeight).toBe("600");
    expect(text.style.lineHeight).toBe("1.4");
    expect(text.style.backgroundColor).not.toBe("");
    expect(text.style.borderRadius).toBe("8px");
    expect(text.style.opacity).toBe("0.8");
  });

  it("renders portable typography, spacing and borderless action styles", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, { document: dom.window.document, HTMLElement: dom.window.HTMLElement });
    const surface = new Surface();
    surface.reset([{ version: "v0.9", updateComponents: { surfaceId: "default", components: [
      { id: "root", component: "Row", align: "center", children: ["price", "action"] },
      { id: "price", component: "Text", variant: "caption", text: "409", styles: {
        "text-decoration": "line-through", "margin-left": "8px", "font-size": "13px",
      } },
      { id: "actionText", component: "Text", text: "查看更多", styles: { color: "#666" } },
      { id: "action", component: "Button", variant: "borderless", child: "actionText" },
    ] } }] as AGenUIMessage[]);
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    const price = container.querySelector('[data-component-id="price"]') as HTMLElement;
    const action = container.querySelector('[data-component-id="action"]') as HTMLButtonElement;
    expect(price.style.textDecoration).toBe("line-through");
    expect(price.style.marginLeft).toBe("8px");
    expect(price.classList.contains("agenui-hint-caption")).toBe(true);
    expect(action.classList.contains("agenui-button-borderless")).toBe(true);
  });

  it("lets presentation styles override a component layout default", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, { document: dom.window.document, HTMLElement: dom.window.HTMLElement });
    const surface = new Surface();
    surface.reset([{ version: "v0.9", updateComponents: { surfaceId: "default", components: [
      { id: "root", component: "Row", align: "start", styles: { "align-items": "flex-end" }, children: ["text"] },
      { id: "text", component: "Text", text: "内容" },
    ] } }] as AGenUIMessage[]);
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    expect((container.firstElementChild as HTMLElement).style.alignItems).toBe("flex-end");
  });

  it("renders List template children using the source path field", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, {
      document: dom.window.document,
      HTMLElement: dom.window.HTMLElement,
    });

    const surface = new Surface();
    surface.reset([
      {
        version: "v0.9",
        updateComponents: {
          surfaceId: "default",
          components: [
            {
              id: "root",
              component: "List",
              children: { componentId: "item", path: "/products" },
            },
            { id: "item", component: "Text", text: { path: "/name" } },
          ],
        },
      },
      {
        version: "v0.9",
        updateDataModel: {
          surfaceId: "default",
          path: "/products",
          value: [{ name: "商品 A" }, { name: "商品 B" }],
        },
      },
    ] as AGenUIMessage[]);

    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    expect(container.textContent).toContain("商品 A");
    expect(container.textContent).toContain("商品 B");
  });

  it("suppresses only the final direct template divider in a List", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, { document: dom.window.document, HTMLElement: dom.window.HTMLElement });
    const surface = new Surface();
    surface.reset([{ version: "v0.9", updateComponents: { surfaceId: "default", components: [
      { id: "root", component: "List", children: { componentId: "item", path: "/items" } },
      { id: "item", component: "Column", children: ["name", "separator"] },
      { id: "name", component: "Text", text: { path: "name" } },
      { id: "separator", component: "Divider" },
    ] } }, { version: "v0.9", updateDataModel: { surfaceId: "default", value: { items: [{ name: "A" }, { name: "B" }] } } }] as AGenUIMessage[]);
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    expect(container.querySelectorAll("hr")).toHaveLength(1);
  });

  it("interpolates catalog formatString expressions inside a repeated item scope", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, {
      document: dom.window.document,
      HTMLElement: dom.window.HTMLElement,
    });

    const surface = new Surface();
    surface.reset([
      {
        version: "v0.9",
        updateComponents: {
          surfaceId: "default",
          components: [
            {
              id: "root",
              component: "List",
              children: { componentId: "price", path: "/items" },
            },
            {
              id: "price",
              component: "Text",
              text: { call: "formatString", args: { value: "¥${current_price}起" } },
            },
          ],
        },
      },
      {
        version: "v0.9",
        updateDataModel: {
          surfaceId: "default",
          path: "/items",
          value: [{ current_price: 198 }, { current_price: 128 }],
        },
      },
    ] as AGenUIMessage[]);

    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    expect(container.textContent).toBe("¥198起¥128起");
  });

  it("renders every component in the basic AGenUI catalog without fallback", () => {
    const dom = new JSDOM('<div id="root"></div>');
    Object.assign(globalThis, { document: dom.window.document, HTMLElement: dom.window.HTMLElement });
    const surface = new Surface();
    surface.reset([{ version: "v0.9", updateComponents: { surfaceId: "default", components: [
      { id: "root", component: "Column", children: ["icon", "image", "video", "audio", "card", "divider", "field", "checkbox", "list", "picker", "slider", "date", "tabs", "modal"] },
      { id: "icon", component: "Icon", name: "check" }, { id: "video", component: "Video", url: "/video.mp4" },
      { id: "image", component: "Image", url: "/image.png" }, { id: "card", component: "Card", child: "card_text" }, { id: "card_text", component: "Text", text: "卡片" },
      { id: "divider", component: "Divider" }, { id: "field", component: "TextField", label: "输入" }, { id: "checkbox", component: "CheckBox", label: "同意", value: true },
      { id: "list", component: "List", children: { componentId: "list_item", path: "/items" } }, { id: "list_item", component: "Text", text: "列表" },
      { id: "audio", component: "AudioPlayer", url: "/audio.mp3", description: "说明" },
      { id: "picker", component: "ChoicePicker", value: ["a"], options: [{ label: "A", value: "a" }] },
      { id: "slider", component: "Slider", value: 3, max: 10 }, { id: "date", component: "DateTimeInput", value: "2026-08-27", enableDate: true },
      { id: "tabs", component: "Tabs", tabs: [{ title: "标签", child: "tab_text" }] }, { id: "tab_text", component: "Text", text: "标签内容" },
      { id: "modal", component: "Modal", trigger: "trigger", content: "content" },
      { id: "trigger", component: "Button", child: "trigger_text", action: {} }, { id: "trigger_text", component: "Text", text: "打开" },
      { id: "content", component: "Text", text: "弹窗内容" },
    ] } }] as AGenUIMessage[]);
    const container = dom.window.document.querySelector("#root") as HTMLElement;
    renderSurface(container, surface);
    expect(container.querySelector(".agenui-fallback")).toBeNull();
    expect(container.querySelector("video")?.getAttribute("src")).toBe("/video.mp4");
    expect(container.querySelector("audio")?.getAttribute("src")).toBe("/audio.mp3");
    expect(container.querySelector('input[type="range"]')).not.toBeNull();
    expect(container.querySelector('input[type="date"]')).not.toBeNull();
    (container.querySelector('[data-component-id="trigger"]') as HTMLButtonElement).click();
    expect(container.querySelector("dialog")?.hasAttribute("open")).toBe(true);
  });
});
