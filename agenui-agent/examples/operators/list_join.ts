interface JoinParams {
  separator?: string;
  field?: string;
}

function run(value: unknown, params?: JoinParams): string {
  if (!Array.isArray(value)) {
    throw new TypeError("list.join expects an array");
  }
  const values = value.map((item) => {
    if (!params?.field) return item;
    if (item === null || typeof item !== "object" || !(params.field in item)) {
      throw new TypeError("list.join field is missing");
    }
    return (item as Record<string, unknown>)[params.field];
  });
  return values.map((item) => String(item)).join(params?.separator ?? "");
}
