package reports

import "testing"

func TestSummariseCase00(t *testing.T) {
	n, sum := Summarise([]int{0, 1})
	if n != 2 || sum != 1 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase01(t *testing.T) {
	n, sum := Summarise([]int{1, 2})
	if n != 2 || sum != 3 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase02(t *testing.T) {
	n, sum := Summarise([]int{2, 3})
	if n != 2 || sum != 5 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase03(t *testing.T) {
	n, sum := Summarise([]int{3, 4})
	if n != 2 || sum != 7 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase04(t *testing.T) {
	n, sum := Summarise([]int{4, 5})
	if n != 2 || sum != 9 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase05(t *testing.T) {
	n, sum := Summarise([]int{5, 6})
	if n != 2 || sum != 11 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase06(t *testing.T) {
	n, sum := Summarise([]int{6, 7})
	if n != 2 || sum != 13 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase07(t *testing.T) {
	n, sum := Summarise([]int{7, 8})
	if n != 2 || sum != 15 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase08(t *testing.T) {
	n, sum := Summarise([]int{8, 9})
	if n != 2 || sum != 17 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase09(t *testing.T) {
	n, sum := Summarise([]int{9, 10})
	if n != 2 || sum != 19 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase10(t *testing.T) {
	n, sum := Summarise([]int{10, 11})
	if n != 2 || sum != 21 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase11(t *testing.T) {
	n, sum := Summarise([]int{11, 12})
	if n != 2 || sum != 23 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase12(t *testing.T) {
	n, sum := Summarise([]int{12, 13})
	if n != 2 || sum != 25 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase13(t *testing.T) {
	n, sum := Summarise([]int{13, 14})
	if n != 2 || sum != 27 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase14(t *testing.T) {
	n, sum := Summarise([]int{14, 15})
	if n != 2 || sum != 29 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase15(t *testing.T) {
	n, sum := Summarise([]int{15, 16})
	if n != 2 || sum != 31 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase16(t *testing.T) {
	n, sum := Summarise([]int{16, 17})
	if n != 2 || sum != 33 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase17(t *testing.T) {
	n, sum := Summarise([]int{17, 18})
	if n != 2 || sum != 35 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase18(t *testing.T) {
	n, sum := Summarise([]int{18, 19})
	if n != 2 || sum != 37 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase19(t *testing.T) {
	n, sum := Summarise([]int{19, 20})
	if n != 2 || sum != 39 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase20(t *testing.T) {
	n, sum := Summarise([]int{20, 21})
	if n != 2 || sum != 41 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase21(t *testing.T) {
	n, sum := Summarise([]int{21, 22})
	if n != 2 || sum != 43 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase22(t *testing.T) {
	n, sum := Summarise([]int{22, 23})
	if n != 2 || sum != 45 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase23(t *testing.T) {
	n, sum := Summarise([]int{23, 24})
	if n != 2 || sum != 47 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase24(t *testing.T) {
	n, sum := Summarise([]int{24, 25})
	if n != 2 || sum != 49 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase25(t *testing.T) {
	n, sum := Summarise([]int{25, 26})
	if n != 2 || sum != 51 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase26(t *testing.T) {
	n, sum := Summarise([]int{26, 27})
	if n != 2 || sum != 53 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase27(t *testing.T) {
	n, sum := Summarise([]int{27, 28})
	if n != 2 || sum != 55 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase28(t *testing.T) {
	n, sum := Summarise([]int{28, 29})
	if n != 2 || sum != 57 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase29(t *testing.T) {
	n, sum := Summarise([]int{29, 30})
	if n != 2 || sum != 59 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase30(t *testing.T) {
	n, sum := Summarise([]int{30, 31})
	if n != 2 || sum != 61 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase31(t *testing.T) {
	n, sum := Summarise([]int{31, 32})
	if n != 2 || sum != 63 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase32(t *testing.T) {
	n, sum := Summarise([]int{32, 33})
	if n != 2 || sum != 65 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase33(t *testing.T) {
	n, sum := Summarise([]int{33, 34})
	if n != 2 || sum != 67 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase34(t *testing.T) {
	n, sum := Summarise([]int{34, 35})
	if n != 2 || sum != 69 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase35(t *testing.T) {
	n, sum := Summarise([]int{35, 36})
	if n != 2 || sum != 71 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase36(t *testing.T) {
	n, sum := Summarise([]int{36, 37})
	if n != 2 || sum != 73 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase37(t *testing.T) {
	n, sum := Summarise([]int{37, 38})
	if n != 2 || sum != 75 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase38(t *testing.T) {
	n, sum := Summarise([]int{38, 39})
	if n != 2 || sum != 77 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase39(t *testing.T) {
	n, sum := Summarise([]int{39, 40})
	if n != 2 || sum != 79 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase40(t *testing.T) {
	n, sum := Summarise([]int{40, 41})
	if n != 2 || sum != 81 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase41(t *testing.T) {
	n, sum := Summarise([]int{41, 42})
	if n != 2 || sum != 83 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase42(t *testing.T) {
	n, sum := Summarise([]int{42, 43})
	if n != 2 || sum != 85 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase43(t *testing.T) {
	n, sum := Summarise([]int{43, 44})
	if n != 2 || sum != 87 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase44(t *testing.T) {
	n, sum := Summarise([]int{44, 45})
	if n != 2 || sum != 89 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase45(t *testing.T) {
	n, sum := Summarise([]int{45, 46})
	if n != 2 || sum != 91 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase46(t *testing.T) {
	n, sum := Summarise([]int{46, 47})
	if n != 2 || sum != 93 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase47(t *testing.T) {
	n, sum := Summarise([]int{47, 48})
	if n != 2 || sum != 95 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase48(t *testing.T) {
	n, sum := Summarise([]int{48, 49})
	if n != 2 || sum != 97 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase49(t *testing.T) {
	n, sum := Summarise([]int{49, 50})
	if n != 2 || sum != 99 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase50(t *testing.T) {
	n, sum := Summarise([]int{50, 51})
	if n != 2 || sum != 101 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase51(t *testing.T) {
	n, sum := Summarise([]int{51, 52})
	if n != 2 || sum != 103 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase52(t *testing.T) {
	n, sum := Summarise([]int{52, 53})
	if n != 2 || sum != 105 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase53(t *testing.T) {
	n, sum := Summarise([]int{53, 54})
	if n != 2 || sum != 107 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase54(t *testing.T) {
	n, sum := Summarise([]int{54, 55})
	if n != 2 || sum != 109 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase55(t *testing.T) {
	n, sum := Summarise([]int{55, 56})
	if n != 2 || sum != 111 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase56(t *testing.T) {
	n, sum := Summarise([]int{56, 57})
	if n != 2 || sum != 113 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase57(t *testing.T) {
	n, sum := Summarise([]int{57, 58})
	if n != 2 || sum != 115 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase58(t *testing.T) {
	n, sum := Summarise([]int{58, 59})
	if n != 2 || sum != 117 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}

func TestSummariseCase59(t *testing.T) {
	n, sum := Summarise([]int{59, 60})
	if n != 2 || sum != 119 {
		t.Fatalf("Summarise = %d, %d", n, sum)
	}
}
