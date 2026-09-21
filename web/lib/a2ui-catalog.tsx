'use client';

import { useState, type CSSProperties } from 'react';
import {
  basicCatalog,
  createComponentImplementation,
  useComponentInspectorAttributes,
  useComponentStyles,
} from '@agenui/react';
import { Catalog } from '@agenui/web-core';
import { ImageApi } from '@agenui/web-core/basic_catalog';

type ImageProps = {
  url?: string;
  fit?: string;
  description?: string;
  variant?: string;
  styles?: Record<string, unknown>;
  weight?: number;
};

/**
 * Factory composer 用 CustomImage 替换 basicCatalog.Image：
 * `fit: scaleDown` 映射到 CSS `scale-down`，失败时显示占位。
 */
const CustomImage = createComponentImplementation(ImageApi, ({ props }: { props: ImageProps }) => {
  const inspectorAttributes = useComponentInspectorAttributes();
  const [hasError, setHasError] = useState(false);
  const mapFit = (fit?: string) => (fit === 'scaleDown' ? 'scale-down' : fit || 'fill');
  const { style: resolvedStyle } = useComponentStyles('Image', props.variant, props.styles, props.weight);
  const style: CSSProperties = {
    objectFit: mapFit(props.fit) as CSSProperties['objectFit'],
    display: 'block',
    boxSizing: 'border-box',
    ...resolvedStyle,
  };
  if (resolvedStyle.aspectRatio) {
    style.aspectRatio = resolvedStyle.aspectRatio;
  }

  if (hasError || !props.url) {
    return (
      <div
        {...inspectorAttributes}
        style={{
          ...style,
          display: 'flex',
          alignItems: 'center',
          justifyContent: 'center',
          backgroundColor: '#f5f5f5',
          overflow: 'hidden',
        }}
      >
        <svg width="32" height="32" viewBox="0 0 24 24" fill="none" stroke="#ccc" strokeWidth="1.5" strokeLinecap="round" strokeLinejoin="round">
          <rect x="3" y="3" width="18" height="18" rx="2" ry="2" />
          <circle cx="8.5" cy="8.5" r="1.5" />
          <polyline points="21 15 16 10 5 21" />
        </svg>
      </div>
    );
  }
  return (
    <img
      {...inspectorAttributes}
      src={props.url}
      alt={props.description || ''}
      style={style}
      onError={() => setHasError(true)}
    />
  );
});

const catalogComponents = [
  ...Array.from(basicCatalog.components.values()).filter((component) => component.name !== 'Image'),
  CustomImage,
];

export const mergedBasicCatalog = new Catalog(
  basicCatalog.id,
  catalogComponents,
  Array.from(basicCatalog.functions.values()),
);
