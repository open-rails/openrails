package message

import (
	"encoding/xml"
	"strings"
)

// Json is a shortcut for map[string]any
type Json map[string]any

// MarshalXML allows type Json to be used with xml.Marshal.
func (j Json) MarshalXML(e *xml.Encoder, start xml.StartElement) error {
	start.Name = xml.Name{
		Space: "",
		Local: "map",
	}
	if err := e.EncodeToken(start); err != nil {
		return err
	}
	for key, value := range j {
		elem := xml.StartElement{
			Name: xml.Name{Space: "", Local: key},
			Attr: []xml.Attr{},
		}
		if err := e.EncodeElement(value, elem); err != nil {
			return err
		}
	}

	return e.EncodeToken(xml.EndElement{Name: start.Name})
}

// Message creates a unified message response structure with consistent "message" field.
// This ensures all responses across the application use the same JSON structure
// as the existing Request.ErrorJSON() method.
//
// Usage: c.JSON(http.StatusBadRequest, json.Message("Invalid request"))
func Message(message string) Json {
	return Json{
		"message": message,
	}
}

// Error is a convenience alias for Message for shorter syntax.
// Usage: c.JSON(http.StatusBadRequest, json.Error("Invalid request"))
func Error(message string) Json {
	return Message(message)
}

// ValidationError creates a user-friendly validation error message.
// It strips internal "validation failed:" prefixes to provide cleaner messages to users.
// Usage: c.JSON(http.StatusBadRequest, json.ValidationError(err))
func ValidationError(err error) Json {
	msg := err.Error()
	// Remove internal prefixes to make the message more user-friendly
	msg = strings.TrimPrefix(msg, "validation failed: ")
	return Message(msg)
}
