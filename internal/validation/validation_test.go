package validation

import "testing"

func TestValidateRegister(t *testing.T) {
	good := RegisterInput{Name: "  Sakib  ", Email: "  Sakib@Example.COM ", Password: "password1", ConfirmPassword: "password1", Role: "SELLER"}
	out, errs := ValidateRegister(good)
	if len(errs) != 0 {
		t.Fatalf("unexpected errors: %v", errs)
	}
	if out.Name != "Sakib" || out.Email != "sakib@example.com" {
		t.Errorf("not normalized: %+v", out)
	}

	cases := []struct {
		name   string
		mutate func(*RegisterInput)
		field  string
		msg    string
	}{
		{"short name", func(i *RegisterInput) { i.Name = "A" }, "name", "Name must be at least 2 characters"},
		{"blank name", func(i *RegisterInput) { i.Name = "   " }, "name", "Name must be at least 2 characters"},
		{"bad email", func(i *RegisterInput) { i.Email = "not-an-email" }, "email", "Please enter a valid email address"},
		{"email with display name", func(i *RegisterInput) { i.Email = "Bob <bob@example.com>" }, "email", "Please enter a valid email address"},
		{"email no tld", func(i *RegisterInput) { i.Email = "bob@localhost" }, "email", "Please enter a valid email address"},
		{"short password", func(i *RegisterInput) { i.Password = "short"; i.ConfirmPassword = "short" }, "password", "Password must be at least 8 characters"},
		{"mismatch", func(i *RegisterInput) { i.ConfirmPassword = "different1" }, "confirmPassword", "Passwords do not match"},
		{"missing role", func(i *RegisterInput) { i.Role = "" }, "role", "Please select an account type"},
		{"admin role rejected", func(i *RegisterInput) { i.Role = "ADMIN" }, "role", "Please select an account type"},
		{"lowercase role rejected", func(i *RegisterInput) { i.Role = "seller" }, "role", "Please select an account type"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			in := RegisterInput{Name: "Sakib", Email: "a@example.com", Password: "password1", ConfirmPassword: "password1", Role: "BUYER"}
			c.mutate(&in)
			_, errs := ValidateRegister(in)
			if errs[c.field] != c.msg {
				t.Errorf("field %s: got %q want %q (all: %v)", c.field, errs[c.field], c.msg, errs)
			}
		})
	}
}

func TestPasswordUpperBound(t *testing.T) {
	long := make([]byte, 129)
	for i := range long {
		long[i] = 'a'
	}
	in := RegisterInput{Name: "Sakib", Email: "a@example.com", Password: string(long), ConfirmPassword: string(long), Role: "BUYER"}
	if _, errs := ValidateRegister(in); errs["password"] == "" {
		t.Error("129-byte password should be rejected")
	}
}

func TestValidateLogin(t *testing.T) {
	out, errs := ValidateLogin(LoginInput{Email: " A@Example.com ", Password: "x"})
	if len(errs) != 0 || out.Email != "a@example.com" {
		t.Fatalf("unexpected: %+v %v", out, errs)
	}
	if _, errs := ValidateLogin(LoginInput{Email: "nope", Password: ""}); errs["email"] == "" || errs["password"] == "" {
		t.Errorf("expected both field errors, got %v", errs)
	}
}

func TestErrsErr(t *testing.T) {
	empty := Errors{}
	if empty.Err() != nil {
		t.Error("empty Errors must produce a nil error")
	}
	full := Errors{"a": "b"}
	if full.Err() == nil {
		t.Error("non-empty Errors must produce an error")
	}
}
