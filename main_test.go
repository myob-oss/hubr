package main_test

import "testing"

func Test_For_CI(t *testing.T) {

    total := 5 + 5
    if total != 10 {
       t.Errorf("Sum was incorrect, got: %d, want: %d.", total, 10)
    }
	
}
