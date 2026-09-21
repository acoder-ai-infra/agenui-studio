interface FilterParams {
  field: string;
  operator: "eq" | "neq" | "gt" | "gte" | "lt" | "lte" | "in" | "contains";
  value: unknown;
}

function run(value: unknown, params?: FilterParams): unknown[] {
  if (!Array.isArray(value) || !params?.field || !params.operator) {
    throw new TypeError("list.filter expects an array and filter parameters");
  }
  return value.filter((item) => {
    if (item === null || typeof item !== "object" || !(params.field in item)) {
      throw new TypeError("list.filter field is missing");
    }
    const candidate = (item as Record<string, unknown>)[params.field];
    switch (params.operator) {
      case "eq": return candidate === params.value;
      case "neq": return candidate !== params.value;
      case "gt": return (candidate as number) > (params.value as number);
      case "gte": return (candidate as number) >= (params.value as number);
      case "lt": return (candidate as number) < (params.value as number);
      case "lte": return (candidate as number) <= (params.value as number);
      case "in":
        if (!Array.isArray(params.value)) throw new TypeError("list.filter in expects an array value");
        return params.value.includes(candidate);
      case "contains":
        if (typeof candidate === "string") return candidate.includes(String(params.value));
        if (Array.isArray(candidate)) return candidate.includes(params.value);
        throw new TypeError("list.filter contains expects a string or array field");
      default: throw new TypeError("list.filter operator is unsupported");
    }
  });
}
