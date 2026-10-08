from agent.quantity import cpu_m, mem_mi, pct


def test_quantities():
    assert cpu_m("250m") == 250 and cpu_m("1") == 1000 and cpu_m("0.5") == 500 and cpu_m("1500000n") == 1.5 and cpu_m("") is None
    assert mem_mi("512Mi") == 512 and mem_mi("1Gi") == 1024 and round(mem_mi("134217728")) == 128 and mem_mi("32Mi") == 32


def test_percentiles():
    assert pct([1, 2, 3, 4, 5], 0.5) == 3 and pct([], 0.9) is None and pct([7], 0.95) == 7
