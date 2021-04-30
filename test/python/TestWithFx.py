from testmodule import *
import sys

class TestWrites(TestRunner):
		
	def __init__(self):
		super().__init__()
	
	def mthd(self):
	
		import sharkbite
		
		securityOps = super().getSecurityOperations()

		auths = sharkbite.Authorizations()
		auths.addAuthorization("blah1")
		auths.addAuthorization("blah2")
	
		securityOps.grantAuthorizations(auths,"root")


		tableOperations = super().getTableOperations()
		tableOperations.create(False)  

		## validate that we DO see the permissions
		
		""" Add authorizations """ 
		""" mutation.put("cf","cq","cv",1569786960) """
		
		with tableOperations.createWriter(auths, 10) as writer:
			mutation = sharkbite.Mutation("row2")
			mutation.put("cf","cq","blah1",1569786960, "value")
			mutation.put("cf2","cq2","blah1",1569786960, "value2")
			""" no value """
			mutation.put("cf3","cq3","blah2",1569786960, "") 
			writer.addMutation( mutation )
			mutation = sharkbite.Mutation("row4")
			mutation.put("cf","cq","blah1",1569786960, "value")
			mutation.put("cf2","cq2","blah1",1569786960, "value2")
			""" no value """
			mutation.put("cf3","cq3","blah2",1569786960, "") 
			writer.addMutation( mutation )
		
		
		auths = sharkbite.Authorizations()
		
		auths.addAuthorization("blah1")
		auths.addAuthorization("blah2")
		count=0
		with tableOperations.createScanner(auths, 2).withRange(sharkbite.Range("row",True,"row3",False)) as scanner:
			resultset = scanner.getResultSet()
			
			for keyvalue in resultset:
				count=count+1
				key = keyvalue.getKey()
				assert( "row2" == key.getRow() )
				value = keyvalue.getValue()
				
				if "cf" == key.getColumnFamily():
					assert( "value"  == value.get() )
				if ("cf2" == key.getColumnFamily() ):
					assert( "value2" == value.get() )
		with tableOperations.createScanner(auths, 2).withRange(sharkbite.Range("row4",True,"row5",False)) as scanner:
			resultset = scanner.getResultSet()
			
			for keyvalue in resultset:
				count=count+1
				key = keyvalue.getKey()
				print("%r %r" % (keyvalue.getKey(), str(keyvalue.getValue())))
				assert( "row4" == key.getRow() )
				value = keyvalue.getValue()
				if "cf" == key.getColumnFamily():
					assert( "value"  == value.get() )
				if ("cf2" == key.getColumnFamily() ):
					assert( "value2" == value.get() )		    
		
		""" delete your table if user did not create temp """
		tableOperations.remove()
		print("count is " + str(count))
		assert(count==6)


runner = TestWrites()
runner.mthd()