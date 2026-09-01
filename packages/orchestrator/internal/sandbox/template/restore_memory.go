package template

type memoryImageProvider interface {
	memoryImagePath() (string, bool)
}

type restoreMemoryTemplate struct {
	Template
	allowFileBackend bool
}

func withRestoreMemoryPolicy(t Template, allowFileBackend bool) Template {
	return &restoreMemoryTemplate{
		Template:         t,
		allowFileBackend: allowFileBackend,
	}
}

func (t *restoreMemoryTemplate) memoryImagePath() (string, bool) {
	if !t.allowFileBackend {
		return "", false
	}

	provider, ok := t.Template.(memoryImageProvider)
	if !ok {
		return "", false
	}

	return provider.memoryImagePath()
}

// FileMemoryBackendPath returns a fully published shared image only when this
// specific restore is eligible. An unavailable image means the caller must use
// UFFD; it is not an error because background materialization is asynchronous.
func FileMemoryBackendPath(t Template) (string, bool) {
	provider, ok := t.(memoryImageProvider)
	if !ok {
		return "", false
	}

	return provider.memoryImagePath()
}
