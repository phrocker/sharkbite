CPPFLAGS=-g --std=c++11 -shared -fPIC
LDFLAGS=-g
LDLIBS=
RM=rm -f


OBJS=$(subst .cpp,.o,$(wildcard *.cpp))


all: streaming

clean: 
	$(RM) $(OBJS)


streaming:	$(OBJS)
	g++ -o streaming.so $(OBJS) $(LDFLAGS) -shared -fPIC
	$(RM) $(OBJS)
