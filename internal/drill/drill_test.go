package drill

import "testing"

func TestReconcileExact(t *testing.T) {
	exp := map[string]int64{"users": 10, "orders": 5}
	if _, err := Reconcile(exp, true, map[string]int64{"users": 10, "orders": 5}, 0); err != nil {
		t.Fatal(err)
	}
	if _, err := Reconcile(exp, true, map[string]int64{"users": 10, "orders": 4}, 0); err == nil {
		t.Fatal("exact mismatch must fail")
	}
	if _, err := Reconcile(exp, true, map[string]int64{"users": 10}, 0); err == nil {
		t.Fatal("missing table must fail")
	}
}

func TestReconcileEstimates(t *testing.T) {
	exp := map[string]int64{"events": 100000, "tiny": 3}
	if _, err := Reconcile(exp, false, map[string]int64{"events": 91000, "tiny": 0}, 0.2); err != nil {
		t.Fatalf("within tolerance should pass: %v", err)
	}
	if _, err := Reconcile(exp, false, map[string]int64{"events": 1000, "tiny": 3}, 0.2); err == nil {
		t.Fatal("99% fewer rows must fail")
	}
}

func TestMySQLImage(t *testing.T) {
	cases := map[string]string{
		"8.0.36":                    "mysql:8.0",
		"8.4.2":                     "mysql:8.4",
		"10.11.6-MariaDB-0+deb12u1": "mariadb:10.11",
		"11.4.2-MariaDB-ubu2404":    "mariadb:11.4",
		"":                          "mysql:8.4",
	}
	for in, want := range cases {
		if got := MySQLImage(in); got != want {
			t.Errorf("MySQLImage(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestTruthy(t *testing.T) {
	for _, v := range []string{"1", "t", "true", "42", "yes please"} {
		if !truthy(v) {
			t.Errorf("%q should be truthy", v)
		}
	}
	for _, v := range []string{"0", "f", "false", "", "NULL", "0.0"} {
		if truthy(v) {
			t.Errorf("%q should be falsy", v)
		}
	}
}
