export function FormFieldErrors({ errors }: { errors: readonly unknown[] }) {
  const message = errors.find((error) => typeof error === "string")

  return typeof message === "string" ? (
    <p role="alert" className="text-xs text-destructive">
      {message}
    </p>
  ) : null
}
