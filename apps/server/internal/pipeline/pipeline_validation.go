package pipeline

// reusableOutput applies the same decoder used to publish a result before it
// can become a durable checkpoint. A successful model request may still carry
// an invalid artifact that a retry needs to regenerate.
func reusableOutput[T any](decode func(string) (T, error)) func(string) bool {
	return func(raw string) bool {
		_, err := decode(raw)
		return err == nil
	}
}
