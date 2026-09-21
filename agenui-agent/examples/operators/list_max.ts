interface NumericFieldParams {
  field?: string;
}

function run(value: unknown, params?: NumericFieldParams): number {
  if (!Array.isArray(value) || value.length === 0) {
    throw new TypeError("list.max expects a non-empty array");
  }
  const values = value.map((item) => {
    const candidate = params?.field
      ? item !== null && typeof item === "object"
        ? (item as Record<string, unknown>)[params.field]
        : undefined
      : item;
    if (typeof candidate !== "number" || !Number.isFinite(candidate)) {
      throw new TypeError("list.max expects finite numeric values");
    }
    return candidate;
  });
  return Math.max(...values);
}
