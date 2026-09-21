interface NumericFieldParams {
  field?: string;
}

function run(value: unknown, params?: NumericFieldParams): number {
  if (!Array.isArray(value)) {
    throw new TypeError("list.sum expects an array");
  }
  return value.reduce<number>((total, item) => {
    const candidate = params?.field
      ? item !== null && typeof item === "object"
        ? (item as Record<string, unknown>)[params.field]
        : undefined
      : item;
    if (typeof candidate !== "number" || !Number.isFinite(candidate)) {
      throw new TypeError("list.sum expects finite numeric values");
    }
    return total + candidate;
  }, 0);
}
